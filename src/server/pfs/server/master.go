package server

import (
	"context"
	"fmt"
	"path"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/pachyderm/pachyderm/v2/src/internal/backoff"
	"github.com/pachyderm/pachyderm/v2/src/internal/consistenthashing"
	"github.com/pachyderm/pachyderm/v2/src/internal/cronutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/dlock"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/log"
	"github.com/pachyderm/pachyderm/v2/src/internal/middleware/auth"
	"github.com/pachyderm/pachyderm/v2/src/internal/pctx"
	"github.com/pachyderm/pachyderm/v2/src/internal/pfsdb"
	"github.com/pachyderm/pachyderm/v2/src/internal/protoutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/randutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/chunk"
	"github.com/pachyderm/pachyderm/v2/src/internal/storage/fileset"
	"github.com/pachyderm/pachyderm/v2/src/internal/task"
	"github.com/pachyderm/pachyderm/v2/src/internal/transactionenv/txncontext"
	"github.com/pachyderm/pachyderm/v2/src/pfs"
	pfsserver "github.com/pachyderm/pachyderm/v2/src/server/pfs"
)

const (
	masterLockPath = "pfs-master-lock"
)

type Master struct {
	env Env

	driver *driver
}

func NewMaster(ctx context.Context, env Env) (*Master, error) {
	d, err := newDriver(ctx, env)
	if err != nil {
		return nil, err
	}
	return &Master{
		env:    env,
		driver: d,
	}, nil
}

func (m *Master) Run(ctx context.Context) error {
	ctx = auth.AsInternalUser(ctx, "pfs-master")
	return backoff.RetryUntilCancel(ctx, func() error {
		eg, ctx := errgroup.WithContext(ctx)
		trackerPeriod := time.Second * time.Duration(m.env.StorageConfig.StorageGCPeriod)
		if trackerPeriod <= 0 {
			log.Info(ctx, "Skipping Storage GC")
		} else {
			eg.Go(func() error {
				lock := dlock.NewDLock(m.driver.etcdClient, path.Join(m.driver.prefix, masterLockPath, "storage-gc"))
				log.Info(ctx, "Starting Storage GC", zap.Duration("period", trackerPeriod))
				ctx, err := lock.Lock(ctx)
				if err != nil {
					return errors.EnsureStack(err)
				}
				defer func() {
					if err := lock.Unlock(ctx); err != nil {
						log.Error(ctx, "error unlocking in pfs master (storage gc)", zap.Error(err))
					}
				}()
				gc := m.driver.storage.Filesets.NewGC(trackerPeriod)
				return gc.RunForever(pctx.Child(ctx, "storage-gc"))
			})
		}
		chunkPeriod := time.Second * time.Duration(m.env.StorageConfig.StorageChunkGCPeriod)
		if chunkPeriod <= 0 {
			log.Info(ctx, "Skipping Chunk Storage GC")
		} else {
			eg.Go(func() error {
				lock := dlock.NewDLock(m.driver.etcdClient, path.Join(m.driver.prefix, masterLockPath, "chunk-gc"))
				log.Info(ctx, "Starting Chunk Storage GC", zap.Duration("period", chunkPeriod))
				ctx, err := lock.Lock(ctx)
				if err != nil {
					return errors.EnsureStack(err)
				}
				defer func() {
					if err := lock.Unlock(ctx); err != nil {
						log.Error(ctx, "error unlocking in pfs master (chunk gc)", zap.Error(err))
					}
				}()
				gc := chunk.NewGC(m.driver.storage.Chunks, chunkPeriod)
				return gc.RunForever(pctx.Child(ctx, "chunk-gc"))
			})
		}
		eg.Go(func() error {
			return m.watchRepos(ctx)
		})
		return errors.EnsureStack(eg.Wait())
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		log.Error(ctx, "error in pfs master; restarting", zap.Error(err), zap.Duration("retryAfter", d))
		return nil
	})
}

func (m *Master) watchRepos(ctx context.Context) error {
	ctx, cancel := pctx.WithCancel(ctx)
	defer cancel()
	repos := make(map[pfsdb.RepoID]context.CancelFunc)
	defer func() {
		for _, cancel := range repos {
			cancel()
		}
	}()
	ringPrefix := path.Join(randutil.UniqueString(m.driver.prefix), masterLockPath, "ring")
	return consistenthashing.WithRing(ctx, m.driver.etcdClient, ringPrefix,
		func(ctx context.Context, ring *consistenthashing.Ring) error {
			return pfsdb.WatchRepos(ctx, m.env.DB, m.env.Listener,
				func(id pfsdb.RepoID, repoInfo *pfs.RepoInfo) error {
					// On Upsert.
					lockPrefix := fmt.Sprintf("repos/%d", id)
					ctx, cancel := pctx.WithCancel(ctx)
					repos[id] = cancel
					go m.driver.manageRepo(ctx, ring, pfsdb.Repo{ID: id, RepoInfo: repoInfo}, lockPrefix)
					return nil
				},
				func(id pfsdb.RepoID) error {
					// On Delete.
					cancel, ok := repos[id]
					if !ok {
						return nil
					}
					defer func() {
						cancel()
						delete(repos, id)
					}()
					return ring.Unlock(fmt.Sprintf("repos/%d", id))
				})
		})
}

func (d *driver) manageRepo(ctx context.Context, ring *consistenthashing.Ring, repo pfsdb.Repo, lockPrefix string) {
	key := pfsdb.RepoKey(repo.RepoInfo.Repo)
	backoff.RetryUntilCancel(ctx, func() (retErr error) { //nolint:errcheck
		ctx, cancel := pctx.WithCancel(ctx)
		defer cancel()
		var err error
		ctx, err = ring.Lock(ctx, lockPrefix)
		if err != nil {
			return errors.Wrap(err, "locking repo lock")
		}
		defer errors.Invoke1(&retErr, ring.Unlock, lockPrefix, "unlocking repo lock")
		var eg errgroup.Group
		eg.Go(func() error {
			return backoff.RetryUntilCancel(ctx, func() error {
				return d.manageBranches(ctx, repo)
			}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
				log.Error(ctx, "managing branches", zap.String("repo", key), zap.Error(err), zap.Duration("retryAfter", d))
				return nil
			})
		})
		eg.Go(func() error {
			return backoff.RetryUntilCancel(ctx, func() error {
				return d.finishRepoCommits(ctx, repo)
			}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
				log.Error(ctx, "finishing repo commits", zap.Uint64("repo id", uint64(repo.ID)), zap.String("repo", key), zap.Error(err), zap.Duration("retryAfter", d))
				return nil
			})
		})
		return errors.EnsureStack(eg.Wait())
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		log.Error(ctx, "managing repo", zap.String("repo", key), zap.Error(err), zap.Duration("retryAfter", d))
		return nil
	})
}

type cronTrigger struct {
	cancel context.CancelFunc
	spec   string
}

func (d *driver) manageBranches(ctx context.Context, repo pfsdb.Repo) error {
	ctx, cancel := pctx.WithCancel(ctx)
	defer cancel()
	cronTriggers := make(map[pfsdb.BranchID]*cronTrigger)
	defer func() {
		for _, ct := range cronTriggers {
			ct.cancel()
		}
	}()
	return pfsdb.WatchBranchesInRepo(ctx, d.env.DB, d.env.Listener, repo.ID,
		func(id pfsdb.BranchID, branchInfo *pfs.BranchInfo) error {
			return d.manageBranch(ctx, pfsdb.Branch{ID: id, BranchInfo: branchInfo}, cronTriggers)
		},
		func(id pfsdb.BranchID) error {
			if ct, ok := cronTriggers[id]; ok {
				ct.cancel()
				delete(cronTriggers, id)
			}
			return nil
		})
}

func (d *driver) manageBranch(ctx context.Context, branch pfsdb.Branch, cronTriggers map[pfsdb.BranchID]*cronTrigger) error {
	branchInfo := branch.BranchInfo
	key := branch.ID
	// Only create a new goroutine if one doesn't already exist or the spec changed.
	if ct, ok := cronTriggers[key]; ok {
		if branchInfo.Trigger != nil && ct.spec == branchInfo.Trigger.CronSpec {
			return nil
		}
		ct.cancel()
		delete(cronTriggers, key)
	}
	if branchInfo.Trigger == nil || branchInfo.Trigger.CronSpec == "" {
		return nil
	}
	ctx, cancel := pctx.WithCancel(ctx)
	cronTriggers[key] = &cronTrigger{
		cancel: cancel,
		spec:   branchInfo.Trigger.CronSpec,
	}
	go func() {
		backoff.RetryUntilCancel(ctx, func() error { //nolint:errcheck
			return d.runCronTrigger(ctx, branchInfo.Branch)
		}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
			log.Error(ctx, "error running cron trigger", zap.Uint64("branch id", uint64(branch.ID)), zap.String("branch", branchInfo.Branch.Key()), zap.Error(err), zap.Duration("retryAfter", d))
			return nil
		})
	}()
	return nil
}

func (d *driver) runCronTrigger(ctx context.Context, branch *pfs.Branch) error {
	branchInfo, err := d.inspectBranch(ctx, branch)
	if err != nil {
		return err
	}
	schedule, err := cronutil.ParseCronExpression(branchInfo.Trigger.CronSpec)
	if err != nil {
		return err
	}
	// Use the current head commit start time as the previous tick.
	// This prevents the timer from restarting if the master restarts.
	ci, err := d.inspectCommitInfo(ctx, branchInfo.Head, pfs.CommitState_STARTED)
	if err != nil {
		return err
	}
	prev := protoutil.MustTime(ci.Started)
	for {
		next := schedule.Next(prev)
		if next.IsZero() {
			log.Debug(ctx, "no more scheduled ticks; exiting loop")
			return nil
		}
		log.Info(ctx, "waiting for next cron tick", zap.Time("next", next), zap.Time("prev", prev))
		select {
		case <-time.After(time.Until(next)):
		case <-ctx.Done():
			return errors.EnsureStack(context.Cause(ctx))
		}
		if err := d.txnEnv.WithWriteContext(ctx, func(ctx context.Context, txnCtx *txncontext.TransactionContext) error {
			trigBI, err := d.inspectBranchInTransaction(ctx, txnCtx, branchInfo.Branch.Repo.NewBranch(branchInfo.Trigger.Branch))
			if err != nil {
				return err
			}
			branchInfo.Head = trigBI.Head
			if _, err := pfsdb.UpsertBranch(ctx, txnCtx.SqlTx, branchInfo); err != nil {
				return err
			}
			return txnCtx.PropagateBranch(branchInfo.Branch)
		}); err != nil {
			return err
		}
		log.Info(ctx, "cron tick completed", zap.Time("next", next))
		prev = next
	}
}

func (d *driver) finishRepoCommits(ctx context.Context, repo pfsdb.Repo) error {
	return pfsdb.WatchCommitsInRepo(ctx, d.env.DB, d.env.Listener, repo.ID,
		func(id pfsdb.CommitID, ci *pfs.CommitInfo) error {
			return d.finishRepoCommit(ctx, repo, &pfsdb.Commit{ID: id, CommitInfo: ci})
		},
		func(_ pfsdb.CommitID) error {
			// Don't finish commits that are deleted.
			return nil
		},
	)
}

func (d *driver) finishRepoCommit(ctx context.Context, repo pfsdb.Repo, commit *pfsdb.Commit) error {
	commitInfo := commit.CommitInfo
	if commitInfo.Finishing == nil || commitInfo.Finished != nil {
		return nil
	}
	commitHandle := commitInfo.Commit
	cache := d.newCache(pfsdb.CommitKey(commitHandle))
	defer func() {
		if err := cache.clear(ctx); err != nil {
			log.Error(ctx, "errored clearing compaction cache", zap.Error(err))
		}
	}()
	return log.LogStep(ctx, "finishCommit", func(ctx context.Context) error {
		return backoff.RetryUntilCancel(ctx, func() error {
			// In the case where a commit is squashed between retries (commit not found), the master can return.
			if _, err := d.getCommit(ctx, commitHandle); err != nil {
				if pfsserver.IsCommitNotFoundErr(err) {
					return nil
				}
				return err
			}
			// Skip compaction / validation for errored commits.
			if commitInfo.Error != "" {
				return d.finalizeCommit(ctx, commit, "", nil)
			}
			compactor := newCompactor(d.storage.Filesets, d.env.StorageConfig.StorageCompactionMaxFanIn)
			taskDoer := d.env.TaskService.NewDoer(StorageTaskNamespace, commitHandle.Id, cache)
			return errors.Wrap(d.storage.Filesets.WithRenewer(ctx, defaultTTL, func(ctx context.Context, renewer *fileset.Renewer) error {
				start := time.Now()
				// Compacting the diff before getting the total allows us to compose the
				// total file set so that it includes the compacted diff.
				if err := log.LogStep(ctx, "compactDiffFileSet", func(ctx context.Context) error {
					_, err := d.compactDiffFileSet(ctx, compactor, taskDoer, renewer, commit)
					return err
				}); err != nil {
					return err
				}
				var totalId *fileset.ID
				var err error
				if err := log.LogStep(ctx, "compactTotalFileSet", func(ctx context.Context) error {
					totalId, err = d.compactTotalFileSet(ctx, compactor, taskDoer, renewer, commit)
					return err
				}); err != nil {
					return err
				}
				details := &pfs.CommitInfo_Details{
					CompactingTime: durationpb.New(time.Since(start)),
				}
				// Validate the commit.
				start = time.Now()
				var validationError string
				if err := log.LogStep(ctx, "validateCommit", func(ctx context.Context) error {
					var err error
					validationError, details.SizeBytes, err = compactor.Validate(ctx, taskDoer, *totalId)
					return err
				}); err != nil {
					return err
				}
				details.ValidatingTime = durationpb.New(time.Since(start))
				// Finish the commit.
				return d.finalizeCommit(ctx, commit, validationError, details)
			}), "finishing commit with renewer")
		}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
			log.Error(ctx, "error finishing commit", zap.Error(err), zap.Duration("retryAfter", d))
			return nil
		})
	}, zap.Bool("finishing", true), log.Proto("commit", commitInfo.Commit), zap.Uint64("repo id", uint64(repo.ID)), zap.String("repo", repo.RepoInfo.Repo.Key()))
}

func (d *driver) compactDiffFileSet(ctx context.Context, compactor *compactor, doer task.Doer, renewer *fileset.Renewer, commit *pfsdb.Commit) (*fileset.ID, error) {
	id, err := d.commitStore.GetDiffFileSet(ctx, commit)
	if err != nil {
		return nil, errors.EnsureStack(err)
	}
	if err := renewer.Add(ctx, *id); err != nil {
		return nil, err
	}
	diffId, err := compactor.Compact(ctx, doer, []fileset.ID{*id}, defaultTTL)
	if err != nil {
		return nil, err
	}
	return diffId, errors.EnsureStack(d.commitStore.SetDiffFileSet(ctx, commit, *diffId))
}

func (d *driver) compactTotalFileSet(ctx context.Context, compactor *compactor, doer task.Doer, renewer *fileset.Renewer, commit *pfsdb.Commit) (*fileset.ID, error) {
	id, err := d.getFileSet(ctx, commit)
	if err != nil {
		return nil, errors.EnsureStack(err)
	}
	if err := renewer.Add(ctx, *id); err != nil {
		return nil, err
	}
	totalId, err := compactor.Compact(ctx, doer, []fileset.ID{*id}, defaultTTL)
	if err != nil {
		return nil, err
	}
	if err := errors.EnsureStack(d.commitStore.SetTotalFileSet(ctx, commit, *totalId)); err != nil {
		return nil, err
	}
	return totalId, nil
}

func (d *driver) finalizeCommit(ctx context.Context, commit *pfsdb.Commit, validationError string, details *pfs.CommitInfo_Details) error {
	return log.LogStep(ctx, "finalizeCommit", func(ctx context.Context) error {
		return d.txnEnv.WithWriteContext(ctx, func(ctx context.Context, txnCtx *txncontext.TransactionContext) error {
			commit.Finished = txnCtx.Timestamp
			if details == nil {
				details = &pfs.CommitInfo_Details{SizeBytes: commit.SizeBytesUpperBound}
			}
			commit.SizeBytesUpperBound = details.SizeBytes
			commit.Details = details
			if commit.Error == "" {
				commit.Error = validationError
			}
			if err := pfsdb.FinishCommit(ctx, txnCtx.SqlTx, commit.ID, commit.Finished, commit.Error, commit.Details); err != nil {
				return errors.Wrap(err, "finalize commit")
			}
			if commit.Commit.Repo.Type == pfs.UserRepoType {
				txnCtx.FinishJob(commit.CommitInfo)
			}
			if commit.Error == "" {
				return d.triggerCommit(ctx, txnCtx, commit.CommitInfo)
			}
			return nil
		})
	})
}
