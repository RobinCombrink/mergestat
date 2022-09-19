package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v4"
	libgit2 "github.com/libgit2/git2go/v33"
	"github.com/mergestat/fuse/internal/db"
	uuid "github.com/satori/go.uuid"
)

// sendBatchGitRefs uses the pg COPY protocol to send a batch of git refs
func (w *worker) sendBatchGitRefs(ctx context.Context, tx pgx.Tx, j *db.DequeueSyncJobRow, batch []*ref) error {
	inputs := make([][]interface{}, 0, len(batch))
	for _, r := range batch {
		var repoID uuid.UUID
		var err error
		if repoID, err = uuid.FromString(j.RepoID.String()); err != nil {
			return err
		}
		input := []interface{}{repoID, r.FullName.String, r.Name.String}

		if r.Hash.Valid {
			input = append(input, r.Hash.String)
		} else {
			input = append(input, nil)
		}

		if r.Remote.Valid {
			input = append(input, r.Remote.String)
		} else {
			input = append(input, nil)
		}

		if r.Target.Valid {
			input = append(input, r.Target.String)
		} else {
			input = append(input, nil)
		}

		if r.Type.Valid {
			input = append(input, r.Type.String)
		} else {
			input = append(input, nil)
		}

		if r.TagCommitHash.Valid {
			input = append(input, r.TagCommitHash.String)
		} else {
			input = append(input, nil)
		}

		inputs = append(inputs, input)
	}

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"git_refs"}, []string{"repo_id", "full_name", "name", "hash", "remote", "target", "type", "tag_commit_hash"}, pgx.CopyFromRows(inputs)); err != nil {
		return err
	}
	return nil
}

type ref struct {
	FullName      sql.NullString `db:"full_name"`
	Hash          sql.NullString `db:"hash"`
	Name          sql.NullString `db:"name"`
	Remote        sql.NullString `db:"remote"`
	Target        sql.NullString `db:"target"`
	Type          sql.NullString `db:"type"`
	TagCommitHash sql.NullString `db:"tag_commit_hash"`
}

const selectRefs = `SELECT *, (CASE type WHEN 'tag' THEN COALESCE(COMMIT_FROM_TAG(tag), hash) END) AS tag_commit_hash FROM refs(?);`

func (w *worker) handleGitRefs(ctx context.Context, j *db.DequeueSyncJobRow) error {
	l := w.loggerForJob(j)

	// TODO(patrickdevivo) uplift the following os.Getenv call to one place, pass value down as a param
	tmpPath, err := os.MkdirTemp(os.Getenv("GIT_CLONE_PATH"), "mergestat-repo-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmpPath); err != nil {
			w.logger.Err(err).Msgf("error cleaning up repo at: %s, %v", tmpPath, err)
		}
	}()

	var ghToken string
	if ghToken, err = w.fetchGitHubTokenFromDB(ctx); err != nil {
		return err
	}

	var repo *libgit2.Repository
	if repo, err = w.cloneRepo(ghToken, j.Repo, tmpPath, true); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	defer repo.Free()

	// indicate that we're starting query execution
	if err := w.sendBatchLogMessages(ctx, []*syncLog{
		{
			Type:            SyncLogTypeInfo,
			RepoSyncQueueID: j.ID,
			Message:         fmt.Sprintf("starting %v sync for %v", j.SyncType, j.Repo),
		},
	}); err != nil {
		return fmt.Errorf("log messages: %w", err)
	}

	refs := make([]*ref, 0)
	if err = w.mergestat.SelectContext(ctx, &refs, selectRefs, tmpPath); err != nil {
		return err
	}

	l.Info().Msgf("retrieved refs: %d", len(refs))

	var tx pgx.Tx
	if tx, err = w.pool.BeginTx(ctx, pgx.TxOptions{}); err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			if !errors.Is(err, pgx.ErrTxClosed) {
				w.logger.Err(err).Msgf("could not rollback transaction")
			}
		}
	}()

	if _, err := tx.Exec(ctx, "DELETE FROM git_refs WHERE repo_id = $1;", j.RepoID.String()); err != nil {
		return err
	}

	if err := w.sendBatchGitRefs(ctx, tx, j, refs); err != nil {
		return err
	}

	l.Info().Msgf("sent batch of %d refs", len(refs))

	if err := w.db.WithTx(tx).SetSyncJobStatus(ctx, db.SetSyncJobStatusParams{Status: "DONE", ID: j.ID}); err != nil {
		return err
	}

	// indicate that we're finishing query execution
	if err := w.sendBatchLogMessages(ctx, []*syncLog{
		{
			Type:            SyncLogTypeInfo,
			RepoSyncQueueID: j.ID,
			Message:         fmt.Sprintf("finished %v sync for %v", j.SyncType, j.Repo),
		},
	}); err != nil {
		return fmt.Errorf("log messages: %w", err)
	}

	return tx.Commit(ctx)
}
