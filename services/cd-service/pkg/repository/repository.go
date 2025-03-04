/*This file is part of kuberpult.

Kuberpult is free software: you can redistribute it and/or modify
it under the terms of the Expat(MIT) License as published by
the Free Software Foundation.

Kuberpult is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
MIT License for more details.

You should have received a copy of the MIT License
along with kuberpult. If not, see <https://directory.fsf.org/wiki/License:Expat>.

Copyright 2023 freiheit.com*/

package repository

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	v1alpha1 "github.com/freiheit-com/kuberpult/services/cd-service/pkg/argocd/v1alpha1"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/grpc"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/mapper"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/freiheit-com/kuberpult/pkg/auth"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/argocd"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/config"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/fs"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/notify"
	"github.com/freiheit-com/kuberpult/services/cd-service/pkg/sqlitestore"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"github.com/freiheit-com/kuberpult/pkg/logger"
	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/util"
	git "github.com/libgit2/git2go/v34"
)

// A Repository provides a multiple reader / single writer access to a git repository.
type Repository interface {
	Apply(ctx context.Context, transformers ...Transformer) error
	Push(ctx context.Context, pushAction func() error) error
	ApplyTransformersInternal(ctx context.Context, transformers ...Transformer) ([]string, *State, []*TransformerResult, error)
	State() *State
	StateAt(oid *git.Oid) (*State, error)
	Notify() *notify.Notify
}

func defaultBackOffProvider() backoff.BackOff {
	eb := backoff.NewExponentialBackOff()
	eb.MaxElapsedTime = 7 * time.Second
	return backoff.WithMaxRetries(eb, 6)
}

var (
	ddMetrics *statsd.Client
)

type StorageBackend int

const (
	DefaultBackend StorageBackend = 0
	GitBackend     StorageBackend = iota
	SqliteBackend  StorageBackend = iota
)

const (
	maxArgoRequests = 3 // note that this happens inside a request, we cannot retry too much!
)

type repository struct {
	// Mutex gurading the writer
	writeLock    sync.Mutex
	writesDone   uint
	queue        queue
	config       *RepositoryConfig
	credentials  *credentialsStore
	certificates *certificateStore

	repository *git.Repository

	// Mutex guarding head
	headLock sync.Mutex

	notify notify.Notify

	backOffProvider func() backoff.BackOff
}

type RepositoryConfig struct {
	// Mandatory Config
	// the URL used for git checkout, (ssh protocol)
	URL  string
	Path string
	// Optional Config
	Credentials    Credentials
	Certificates   Certificates
	CommitterEmail string
	CommitterName  string
	// default branch is master
	Branch string
	// network timeout
	NetworkTimeout time.Duration
	//
	GcFrequency    uint
	StorageBackend StorageBackend
	// Bootstrap mode controls where configurations are read from
	// true: read from json file at EnvironmentConfigsPath
	// false: read from config files in manifest repo
	BootstrapMode          bool
	EnvironmentConfigsPath string
	ArgoInsecure           bool
	// if set, kuberpult will generate push events to argoCd whenever it writes to the manifest repo:
	ArgoWebhookUrl string
	// the url to the git repo, like the browser requires it (https protocol)
	WebURL string
}

func openOrCreate(path string, storageBackend StorageBackend) (*git.Repository, error) {
	repo2, err := git.OpenRepositoryExtended(path, git.RepositoryOpenNoSearch, path)
	if err != nil {
		var gerr *git.GitError
		if errors.As(err, &gerr) {
			if gerr.Code == git.ErrNotFound {
				os.MkdirAll(path, 0777)
				repo2, err = git.InitRepository(path, true)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if storageBackend == SqliteBackend {
		sqlitePath := filepath.Join(path, "odb.sqlite")
		be, err := sqlitestore.NewOdbBackend(sqlitePath)
		if err != nil {
			return nil, fmt.Errorf("creating odb backend: %w", err)
		}
		odb, err := repo2.Odb()
		if err != nil {
			return nil, fmt.Errorf("gettting odb: %w", err)
		}
		// Prioriority 99 ensures that libgit prefers this backend for writing over its buildin backends.
		err = odb.AddBackend(be, 99)
		if err != nil {
			return nil, fmt.Errorf("setting odb backend: %w", err)
		}
	}
	return repo2, err
}

// Opens a repository. The repository is initialized and updated in the background.
func New(ctx context.Context, cfg RepositoryConfig) (Repository, error) {
	logger := logger.FromContext(ctx)

	ddMetricsFromCtx := ctx.Value("ddMetrics")
	if ddMetricsFromCtx != nil {
		ddMetrics = ddMetricsFromCtx.(*statsd.Client)
	}

	if cfg.Branch == "" {
		cfg.Branch = "master"
	}
	if cfg.CommitterEmail == "" {
		cfg.CommitterEmail = "kuberpult@example.com"
	}
	if cfg.CommitterName == "" {
		cfg.CommitterName = "kuberpult"
	}
	if cfg.StorageBackend == DefaultBackend {
		cfg.StorageBackend = SqliteBackend
	}
	if cfg.NetworkTimeout == 0 {
		cfg.NetworkTimeout = time.Minute
	}
	var credentials *credentialsStore
	var certificates *certificateStore
	var err error
	if strings.HasPrefix(cfg.URL, "./") || strings.HasPrefix(cfg.URL, "/") {
		logger.Debug("git url indicates a local directory. Ignoring credentials and certificates.")
	} else {
		credentials, err = cfg.Credentials.load()
		if err != nil {
			return nil, err
		}
		certificates, err = cfg.Certificates.load()
		if err != nil {
			return nil, err
		}
	}

	if repo2, err := openOrCreate(cfg.Path, cfg.StorageBackend); err != nil {
		return nil, err
	} else {
		// configure remotes
		if remote, err := repo2.Remotes.CreateAnonymous(cfg.URL); err != nil {
			return nil, err
		} else {
			result := &repository{
				config:          &cfg,
				credentials:     credentials,
				certificates:    certificates,
				repository:      repo2,
				queue:           makeQueue(),
				backOffProvider: defaultBackOffProvider,
			}
			result.headLock.Lock()

			defer result.headLock.Unlock()
			fetchSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", cfg.Branch, cfg.Branch)
			fetchOptions := git.FetchOptions{
				RemoteCallbacks: git.RemoteCallbacks{
					UpdateTipsCallback: func(refname string, a *git.Oid, b *git.Oid) error {
						logger.Debug("git.fetched",
							zap.String("refname", refname),
							zap.String("revision.new", b.String()),
						)
						return nil
					},
					CredentialsCallback:      credentials.CredentialsCallback(ctx),
					CertificateCheckCallback: certificates.CertificateCheckCallback(ctx),
				},
			}
			err := remote.Fetch([]string{fetchSpec}, &fetchOptions, "fetching")
			if err != nil {
				return nil, err
			}
			var zero git.Oid
			var rev *git.Oid = &zero
			if remoteRef, err := repo2.References.Lookup(fmt.Sprintf("refs/remotes/origin/%s", cfg.Branch)); err != nil {
				var gerr *git.GitError
				if errors.As(err, &gerr) && gerr.Code == git.ErrNotFound {
					// not found
					// nothing to do
				} else {
					return nil, err
				}
			} else {
				rev = remoteRef.Target()
				if _, err := repo2.References.Create(fmt.Sprintf("refs/heads/%s", cfg.Branch), rev, true, "reset branch"); err != nil {
					return nil, err
				}
			}

			// check that we can build the current state
			state, err := result.StateAt(nil)
			if err != nil {
				return nil, err
			}

			// Check configuration for errors and abort early if any:
			_, err = state.GetEnvironmentConfigsAndValidate(ctx)
			if err != nil {
				return nil, err
			}
			go result.ProcessQueue(ctx)
			return result, nil
		}
	}
}

func (r *repository) ProcessQueue(ctx context.Context) {
	defer func() {
		close(r.queue.elements)
		for e := range r.queue.elements {
			e.result <- ctx.Err()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-r.queue.elements:
			r.ProcessQueueOnce(ctx, e, defaultPushUpdate, DefaultPushActionCallback)
		}
	}
}

func (r *repository) applyElements(elements []element, allowFetchAndReset bool) ([]element, error, *TransformerResult) {
	var changes = &TransformerResult{}
	for i := 0; i < len(elements); {
		e := elements[i]
		subChanges, applyErr := r.ApplyTransformers(e.ctx, e.transformers...)
		changes.Combine(subChanges)
		if applyErr != nil {
			if errors.Is(applyErr, invalidJson) && allowFetchAndReset {
				// Invalid state. fetch and reset and redo
				err := r.FetchAndReset(e.ctx)
				if err != nil {
					return elements, err, nil
				}
				return r.applyElements(elements, false)
			} else {
				e.result <- applyErr
				// here, we keep all elements "behind i".
				// these are the elements that have not been applied yet
				elements = append(elements[:i], elements[i+1:]...)
			}
		} else {
			i++
		}
	}
	return elements, nil, changes
}

var panicError = errors.New("Panic")

func (r *repository) useRemote(ctx context.Context, callback func(*git.Remote) error) error {
	remote, err := r.repository.Remotes.CreateAnonymous(r.config.URL)
	if err != nil {
		return fmt.Errorf("opening remote %q: %w", r.config.URL, err)
	}
	defer remote.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), r.config.NetworkTimeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- callback(remote)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (r *repository) drainQueue() []element {
	elements := []element{}
	for {
		select {
		case f := <-r.queue.elements:
			// Check that the item is not already cancelled
			select {
			case <-f.ctx.Done():
				f.result <- f.ctx.Err()
			default:
				elements = append(elements, f)
			}
		default:
			return elements
		}
	}
}

// It returns always nil
// success is set to true if the push was successful
func defaultPushUpdate(branch string, success *bool) git.PushUpdateReferenceCallback {
	return func(refName string, status string) error {
		var expectedRefName = fmt.Sprintf("refs/heads/%s", branch)
		// if we were successful the status is empty and the ref contains our branch:
		*success = refName == expectedRefName && status == ""
		return nil
	}
}

type PushActionFunc func() error
type PushActionCallbackFunc func(git.PushOptions, *repository) PushActionFunc

// DefaultPushActionCallback is public for testing reasons only.
func DefaultPushActionCallback(pushOptions git.PushOptions, r *repository) PushActionFunc {
	return func() error {
		return r.useRemote(context.Background(), func(remote *git.Remote) error {
			return remote.Push([]string{fmt.Sprintf("refs/heads/%s:refs/heads/%s", r.config.Branch, r.config.Branch)}, &pushOptions)
		})
	}
}

type PushUpdateFunc func(string, *bool) git.PushUpdateReferenceCallback

func (r *repository) ProcessQueueOnce(ctx context.Context, e element, callback PushUpdateFunc, pushAction PushActionCallbackFunc) {
	logger := logger.FromContext(ctx)
	var err error = panicError
	elements := []element{e}
	defer func() {
		for _, el := range elements {
			el.result <- err
		}
	}()
	// Check that the first element is not already canceled
	select {
	case <-e.ctx.Done():
		e.result <- e.ctx.Err()
		return
	default:
	}

	// Try to fetch more items from the queue in order to push more things together
	elements = append(elements, r.drainQueue()...)

	var pushSuccess = true
	pushOptions := git.PushOptions{
		RemoteCallbacks: git.RemoteCallbacks{
			CredentialsCallback:         r.credentials.CredentialsCallback(e.ctx),
			CertificateCheckCallback:    r.certificates.CertificateCheckCallback(e.ctx),
			PushUpdateReferenceCallback: callback(r.config.Branch, &pushSuccess),
		},
	}

	// Apply the items
	elements, err, changes := r.applyElements(elements, true)
	if err != nil {
		return
	}

	if len(elements) == 0 {
		return
	}

	// Try pushing once
	err = r.Push(e.ctx, pushAction(pushOptions, r))
	if err != nil {
		gerr, ok := err.(*git.GitError)
		// If it doesn't work because the branch diverged, try reset and apply again.
		if ok && gerr.Code == git.ErrorCodeNonFastForward {
			err = r.FetchAndReset(e.ctx)
			if err != nil {
				return
			}
			// Apply the items
			elements, err, _ = r.applyElements(elements, false)
			if err != nil || len(elements) == 0 {
				return
			}
			if pushErr := r.Push(e.ctx, pushAction(pushOptions, r)); pushErr != nil {
				err = &InternalError{inner: pushErr}
			}
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			err = grpc.CanceledError(ctx, err)
		} else {
			logger.Error(fmt.Sprintf("error while pushing: %s", err))
			err = grpc.PublicError(ctx, errors.New(fmt.Sprintf("could not push to manifest repository '%s' on branch '%s' - this indicates that the ssh key does not have write access", r.config.URL, r.config.Branch)))
		}
	} else {
		if !pushSuccess {
			err = fmt.Errorf("failed to push - this indicates that branch protection is enabled in '%s' on branch '%s'", r.config.URL, r.config.Branch)
		}
	}
	span, ctx := tracer.StartSpanFromContext(e.ctx, "PostPush")
	defer span.Finish()

	if r.config.ArgoWebhookUrl != "" {
		r.sendWebhookToArgoCd(ctx, logger, changes)
	}

	r.notify.Notify()
}

func (r *repository) sendWebhookToArgoCd(ctx context.Context, logger *zap.Logger, changes *TransformerResult) {
	var modified = []string{}
	for i := range changes.ChangedApps {
		change := changes.ChangedApps[i]
		// we may need to add the root app in some circumstances - so far it doesn't seem necessary, so we just add the manifest.yaml:
		manifestFilename := fmt.Sprintf("environments/%s/applications/%s/manifests/manifests.yaml", change.Env, change.App)
		modified = append(modified, manifestFilename)
		logger.Info(fmt.Sprintf("ArgoWebhookUrl: adding modified: %s", manifestFilename))
	}
	var deleted = []string{}
	for i := range changes.DeletedRootApps {
		change := changes.DeletedRootApps[i]
		// we may need to add the root app in some circumstances - so far it doesn't seem necessary, so we just add the manifest.yaml:
		rootAppFilename := fmt.Sprintf("argocd/%s/%s.yaml", "v1alpha1", change.Env)
		deleted = append(deleted, rootAppFilename)
		logger.Info(fmt.Sprintf("ArgoWebhookUrl: adding modified: %s", rootAppFilename))
	}

	argoResult := ArgoWebhookData{
		htmlUrl:  r.config.WebURL, // if this does not match, argo will completely ignore the request and return 200
		revision: "refs/heads/" + r.config.Branch,
		change: changeInfo{
			payloadAfter: changes.Commits.Current.String(),
		},
		defaultBranch: r.config.Branch, // this is questionable, because we don't actually know the default branch, but it seems to work fine in practice
		Commits: []commit{
			{
				Added:    []string{},
				Modified: modified,
				Removed:  deleted,
			},
		},
	}
	if changes.Commits.Previous != nil {
		argoResult.change.payloadBefore = changes.Commits.Previous.String()
	}

	span, ctx := tracer.StartSpanFromContext(ctx, "Webhook-Retries")
	defer span.Finish()
	success := false
	var err error = nil
	for i := 1; i <= maxArgoRequests; i++ {
		err, shouldRetry := doWebhookPostRequest(ctx, argoResult, r.config, i)
		if err != nil && shouldRetry {
			logger.Warn(fmt.Sprintf("ProcessQueueOnce: error sending webhook on try %d: %v", i, err))
			if shouldRetry {
				// we're still in a request here, we can't wait too long:
				time.Sleep(time.Duration(100*i) * time.Millisecond)
			} else {
				break
			}
		} else {
			logger.Info(fmt.Sprintf("ProcessQueueOnce: argo webhook was send successfully on try %d!", i))
			success = true
			break
		}
	}
	span.SetTag("success", success)
	if !success {
		logger.Error(fmt.Sprintf("ProcessQueueOnce: error sending webhook after all %d tries: %v", maxArgoRequests, err))
	}
}

func contains(s []int, e int) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func doWebhookPostRequest(ctx context.Context, data ArgoWebhookData, repoConfig *RepositoryConfig, retryCounter int) (error, bool) {
	span, ctx := tracer.StartSpanFromContext(ctx, "Webhook")
	span.SetTag("changeAfter", data.change.payloadAfter)
	span.SetTag("changeBefore", data.change.payloadBefore)
	span.SetTag("try", retryCounter)
	defer span.Finish()
	url := repoConfig.ArgoWebhookUrl + "/api/webhook"
	l := logger.FromContext(ctx)
	l.Info(fmt.Sprintf("doWebhookPostRequest: URL: %s", url))

	var argoFormat = v1alpha1.PushPayload{
		Ref:    data.revision,
		Before: data.change.payloadBefore,
		After:  data.change.payloadAfter,
		Repository: v1alpha1.Repository{
			HTMLURL:       data.htmlUrl,
			DefaultBranch: data.defaultBranch,
		},
		Commits: toArgoCommits(data.Commits),
	}

	jsonBytes, err := json.MarshalIndent(argoFormat, " ", " ")
	if err != nil {
		return err, false
	}
	l.Info(fmt.Sprintf("doWebhookPostRequest argo format: %s", string(jsonBytes)))
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	req.Header.Set("Content-Type", "application/json")

	// now pretend that we are GitHub by adding this header, otherwise argo will ignore our request:
	req.Header.Set("X-GitHub-Event", "push")

	tr := &http.Transport{
		// we reach argo from within the cluster, so there's no ssl:
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: repoConfig.ArgoInsecure,
		},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New(fmt.Sprintf("doWebhookPostRequest: could not send request to '%s': %s", url, err.Error())), false
	}
	defer resp.Body.Close()

	//l.Warn(fmt.Sprintf("response Status: %d", resp.StatusCode))
	l.Info(fmt.Sprintf("response headers: %s", resp.Header))
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// weird but we kinda do not care about the body:
		l.Warn(fmt.Sprintf("doWebhookPostRequest: could not read body: %s - continuing anyway", err.Error()))
	}
	validResponseCodes := []int{200}
	if resp.StatusCode >= 500 {
		return errors.New(fmt.Sprintf("doWebhookPostRequest: invalid status code from argo: %d", resp.StatusCode)), true
	}

	if contains(validResponseCodes, resp.StatusCode) {
		l.Info(fmt.Sprintf("doWebhookPostRequest: response Body: %s", string(body)))
		return nil, false
	}
	// in any other case we should not do a retry (e.g. status 4xx):
	l.Warn(fmt.Sprintf("doWebhookPostRequest: response Body: %s", string(body)))
	return errors.New(fmt.Sprintf("doWebhookPostRequest: invalid status code from argo: %d", resp.StatusCode)), false
}

func toArgoCommits(commits []commit) []v1alpha1.Commit {
	var result = []v1alpha1.Commit{}
	for i := range commits {
		c := commits[i]
		result = append(result, v1alpha1.Commit{
			// ArgoCd ignores most fields, so we can ignore them too.
			// Source: function "affectedRevisionInfo" in https://github.com/argoproj/argo-cd/blob/master/util/webhook/webhook.go#L141
			Sha:       "",
			ID:        "",
			NodeID:    "",
			TreeID:    "",
			Distinct:  false,
			Message:   "",
			Timestamp: "",
			URL:       "",
			Author: struct {
				Name     string `json:"name"`
				Email    string `json:"email"`
				Username string `json:"username"`
			}{},
			Committer: struct {
				Name     string `json:"name"`
				Email    string `json:"email"`
				Username string `json:"username"`
			}{},
			Added:    c.Added,
			Removed:  c.Removed,
			Modified: c.Modified,
		})
	}
	return result
}

type changeInfo struct {
	payloadBefore string
	payloadAfter  string
}
type commit struct {
	Added    []string
	Modified []string
	Removed  []string
}

type ArgoWebhookData struct {
	htmlUrl       string
	revision      string // aka "ref"
	change        changeInfo
	defaultBranch string
	Commits       []commit
}

func (r *repository) ApplyTransformersInternal(ctx context.Context, transformers ...Transformer) ([]string, *State, []*TransformerResult, error) {
	if state, err := r.StateAt(nil); err != nil {
		return nil, nil, nil, grpc.InternalError(ctx, fmt.Errorf("%s: %w", "failure in StateAt", err))
	} else {
		var changes []*TransformerResult = nil
		commitMsg := []string{}
		ctxWithTime := withTimeNow(ctx, time.Now())
		for _, t := range transformers {
			if msg, subChanges, err := t.Transform(ctxWithTime, state); err != nil {
				return nil, nil, nil, err
			} else {
				commitMsg = append(commitMsg, msg)
				changes = append(changes, subChanges)
			}
		}
		return commitMsg, state, changes, nil
	}
}

type AppEnv struct {
	App string
	Env string
}

type RootApp struct {
	Env string
	//argocd/v1alpha1/development2.yaml
}

type TransformerResult struct {
	ChangedApps     []AppEnv
	DeletedRootApps []RootApp
	Commits         *CommitIds
}

type CommitIds struct {
	Previous *git.Oid
	Current  *git.Oid
}

func (r *TransformerResult) AddAppEnv(app string, env string) {
	r.ChangedApps = append(r.ChangedApps, AppEnv{
		App: app,
		Env: env,
	})
}

func (r *TransformerResult) AddRootApp(env string) {
	r.DeletedRootApps = append(r.DeletedRootApps, RootApp{
		Env: env,
	})
}

func (r *TransformerResult) Combine(other *TransformerResult) {
	if other == nil {
		return
	}
	for i := range other.ChangedApps {
		a := other.ChangedApps[i]
		r.AddAppEnv(a.App, a.Env)
	}
	for i := range other.DeletedRootApps {
		a := other.DeletedRootApps[i]
		r.AddRootApp(a.Env)
	}
	if r.Commits == nil {
		r.Commits = other.Commits
	}
}

func CombineArray(others []*TransformerResult) *TransformerResult {
	var r *TransformerResult = &TransformerResult{}
	for i := range others {
		r.Combine(others[i])
	}
	return r
}

func (r *repository) ApplyTransformers(ctx context.Context, transformers ...Transformer) (*TransformerResult, error) {
	commitMsg, state, changes, err := r.ApplyTransformersInternal(ctx, transformers...)
	if err != nil {
		return nil, err
	}
	err = UpdateDatadogMetrics(state)
	if err != nil {
		return nil, err
	}
	if err := r.afterTransform(ctx, *state); err != nil {
		return nil, grpc.InternalError(ctx, fmt.Errorf("%s: %w", "failure in afterTransform", err))
	}

	treeId, err := state.Filesystem.(*fs.TreeBuilderFS).Insert()
	if err != nil {
		return nil, &InternalError{inner: err}
	}
	committer := &git.Signature{
		Name:  r.config.CommitterName,
		Email: r.config.CommitterEmail,
		When:  time.Now(),
	}

	user, err := auth.ReadUserFromContext(ctx)

	if err != nil {
		return nil, err
	}

	author := &git.Signature{
		Name:  user.Name,
		Email: user.Email,
		When:  time.Now(),
	}

	var rev *git.Oid
	// the commit can be nil, if it's the first commit in the repo
	if state.Commit != nil {
		rev = state.Commit.Id()
	}
	oldCommitId := rev

	newCommitId, err := r.repository.CreateCommitFromIds(
		fmt.Sprintf("refs/heads/%s", r.config.Branch),
		author,
		committer,
		strings.Join(commitMsg, "\n"),
		treeId,
		rev,
	)
	if err != nil {
		return nil, grpc.InternalError(ctx, fmt.Errorf("%s: %w", "createCommitFromIds failed", err))
	}
	result := CombineArray(changes)
	result.Commits = &CommitIds{
		Current:  newCommitId,
		Previous: nil,
	}
	if oldCommitId != nil {
		result.Commits.Previous = oldCommitId
	}
	return result, nil
}

func (r *repository) FetchAndReset(ctx context.Context) error {
	fetchSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.config.Branch, r.config.Branch)
	logger := logger.FromContext(ctx)
	fetchOptions := git.FetchOptions{
		RemoteCallbacks: git.RemoteCallbacks{
			UpdateTipsCallback: func(refname string, a *git.Oid, b *git.Oid) error {
				logger.Debug("git.fetched",
					zap.String("refname", refname),
					zap.String("revision.new", b.String()),
				)
				return nil
			},
			CredentialsCallback:      r.credentials.CredentialsCallback(ctx),
			CertificateCheckCallback: r.certificates.CertificateCheckCallback(ctx),
		},
	}
	err := r.useRemote(ctx, func(remote *git.Remote) error {
		return remote.Fetch([]string{fetchSpec}, &fetchOptions, "fetching")
	})
	if err != nil {
		return &InternalError{inner: err}
	}
	var zero git.Oid
	var rev *git.Oid = &zero
	if remoteRef, err := r.repository.References.Lookup(fmt.Sprintf("refs/remotes/origin/%s", r.config.Branch)); err != nil {
		var gerr *git.GitError
		if errors.As(err, &gerr) && gerr.Code == git.ErrNotFound {
			// not found
			// nothing to do
		} else {
			return err
		}
	} else {
		rev = remoteRef.Target()
		if _, err := r.repository.References.Create(fmt.Sprintf("refs/heads/%s", r.config.Branch), rev, true, "reset branch"); err != nil {
			return err
		}
	}
	obj, err := r.repository.Lookup(rev)
	if err != nil {
		return err
	}
	commit, err := obj.AsCommit()
	if err != nil {
		return err
	}
	err = r.repository.ResetToCommit(commit, git.ResetSoft, &git.CheckoutOptions{Strategy: git.CheckoutForce})
	if err != nil {
		return err
	}
	return nil
}

func (r *repository) Apply(ctx context.Context, transformers ...Transformer) error {
	defer func() {
		r.writesDone = r.writesDone + uint(len(transformers))
		r.maybeGc(ctx)
	}()
	eCh := r.applyDeferred(ctx, transformers...)
	select {
	case err := <-eCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *repository) applyDeferred(ctx context.Context, transformers ...Transformer) <-chan error {
	return r.queue.add(ctx, transformers)
}

// Push returns an 'error' for typing reasons, really it is always a git.GitError
func (r *repository) Push(ctx context.Context, pushAction func() error) error {

	span, ctx := tracer.StartSpanFromContext(ctx, "Apply")
	defer span.Finish()

	eb := r.backOffProvider()
	return backoff.Retry(
		func() error {
			span, _ := tracer.StartSpanFromContext(ctx, "Push")
			defer span.Finish()
			err := pushAction()
			if err != nil {
				gerr, ok := err.(*git.GitError)
				if ok && gerr.Code == git.ErrorCodeNonFastForward {
					return backoff.Permanent(err)
				}
			}
			return err
		},
		eb,
	)
}

func (r *repository) afterTransform(ctx context.Context, state State) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "afterTransform")
	defer span.Finish()

	configs, err := state.GetEnvironmentConfigs()
	if err != nil {
		return err
	}
	for env, config := range configs {
		if config.ArgoCd != nil {
			err := r.updateArgoCdApps(ctx, &state, env, config)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *repository) updateArgoCdApps(ctx context.Context, state *State, env string, config config.EnvironmentConfig) error {
	fs := state.Filesystem
	if apps, err := state.GetEnvironmentApplications(env); err != nil {
		return err
	} else {
		appData := []argocd.AppData{}
		sort.Strings(apps)
		for _, appName := range apps {
			if err != nil {
				return err
			}
			team, err := state.GetApplicationTeamOwner(appName)
			if err != nil {
				return err
			}
			version, err := state.GetEnvironmentApplicationVersion(env, appName)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// if the app does not exist, we skip it
					// (It may not exist at all, or just hasn't been deployed to this environment yet)
					continue
				}
				return err
			}
			if version == nil || *version == 0 {
				// if nothing is deployed, ignore it
				continue
			}
			appData = append(appData, argocd.AppData{
				AppName:  appName,
				TeamName: team,
			})
		}
		if manifests, err := argocd.Render(r.config.URL, r.config.Branch, config, env, appData); err != nil {
			return err
		} else {
			for apiVersion, content := range manifests {
				if err := fs.MkdirAll(fs.Join("argocd", string(apiVersion)), 0777); err != nil {
					return err
				}
				target := fs.Join("argocd", string(apiVersion), fmt.Sprintf("%s.yaml", env))
				if err := util.WriteFile(fs, target, content, 0666); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *repository) State() *State {
	s, err := r.StateAt(nil)
	if err != nil {
		panic(err)
	}
	return s
}

func (r *repository) StateAt(oid *git.Oid) (*State, error) {
	var commit *git.Commit
	if oid == nil {
		if obj, err := r.repository.RevparseSingle(fmt.Sprintf("refs/heads/%s", r.config.Branch)); err != nil {
			var gerr *git.GitError
			if errors.As(err, &gerr) {
				if gerr.Code == git.ErrNotFound {
					return &State{
						Filesystem:             fs.NewEmptyTreeBuildFS(r.repository),
						BootstrapMode:          r.config.BootstrapMode,
						EnvironmentConfigsPath: r.config.EnvironmentConfigsPath,
					}, nil
				}
			}
			return nil, err
		} else {
			commit, err = obj.AsCommit()
			if err != nil {
				return nil, err
			}
		}
	} else {
		var err error
		commit, err = r.repository.LookupCommit(oid)
		if err != nil {
			return nil, err
		}
	}
	return &State{
		Filesystem:             fs.NewTreeBuildFS(r.repository, commit.TreeId()),
		Commit:                 commit,
		BootstrapMode:          r.config.BootstrapMode,
		EnvironmentConfigsPath: r.config.EnvironmentConfigsPath,
	}, nil
}

func (r *repository) Notify() *notify.Notify {
	return &r.notify
}

type ObjectCount struct {
	Count       uint64
	Size        uint64
	InPack      uint64
	Packs       uint64
	SizePack    uint64
	Garbage     uint64
	SizeGarbage uint64
}

func (r *repository) countObjects(ctx context.Context) (ObjectCount, error) {
	var stats ObjectCount
	/*
		The output of `git count-objects` looks like this:
			count: 0
			size: 0
			in-pack: 635
			packs: 1
			size-pack: 2845
			prune-packable: 0
			garbage: 0
			size-garbage: 0
	*/
	cmd := exec.CommandContext(ctx, "git", "count-objects", "--verbose")
	cmd.Dir = r.config.Path
	out, err := cmd.Output()
	if err != nil {
		return stats, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		var (
			token string
			value uint64
		)
		if _, err := fmt.Sscan(scanner.Text(), &token, &value); err != nil {
			return stats, err
		}
		switch token {
		case "count:":
			stats.Count = value
		case "size:":
			stats.Size = value
		case "in-packs:":
			stats.InPack = value
		case "packs:":
			stats.Packs = value
		case "size-pack:":
			stats.SizePack = value
		case "garbage:":
			stats.Garbage = value
		case "size-garbage":
			stats.SizeGarbage = value
		}
	}
	return stats, nil
}

func (r *repository) maybeGc(ctx context.Context) {
	if r.config.StorageBackend == SqliteBackend || r.config.GcFrequency == 0 || r.writesDone < r.config.GcFrequency {
		return
	}
	log := logger.FromContext(ctx)
	r.writesDone = 0
	timeBefore := time.Now()
	statsBefore, _ := r.countObjects(ctx)
	cmd := exec.CommandContext(ctx, "git", "repack", "-a", "-d")
	cmd.Dir = r.config.Path
	err := cmd.Run()
	if err != nil {
		log.Fatal("git.repack", zap.Error(err))
		return
	}
	statsAfter, _ := r.countObjects(ctx)
	log.Info("git.repack", zap.Duration("duration", time.Now().Sub(timeBefore)), zap.Uint64("collected", statsBefore.Count-statsAfter.Count))
}

type State struct {
	Filesystem             billy.Filesystem
	Commit                 *git.Commit
	BootstrapMode          bool
	EnvironmentConfigsPath string
}

func (s *State) Releases(application string) ([]uint64, error) {
	if entries, err := s.Filesystem.ReadDir(s.Filesystem.Join("applications", application, "releases")); err != nil {
		return nil, err
	} else {
		result := make([]uint64, 0, len(entries))
		for _, e := range entries {
			if i, err := strconv.ParseUint(e.Name(), 10, 64); err != nil {
				// just skip
			} else {
				result = append(result, i)
			}
		}
		return result, nil
	}
}

func (s *State) ReleaseManifests(application string, release uint64) (map[string]string, error) {
	base := s.Filesystem.Join("applications", application, "releases", strconv.FormatUint(release, 10), "environments")
	if entries, err := s.Filesystem.ReadDir(base); err != nil {
		return nil, err
	} else {
		result := make(map[string]string, len(entries))
		for _, e := range entries {
			if buf, err := readFile(s.Filesystem, s.Filesystem.Join(base, e.Name(), "manifests.yaml")); err != nil {
				return nil, err
			} else {
				result[e.Name()] = string(buf)
			}
		}
		return result, nil
	}
}

type Actor struct {
	Name  string
	Email string
}

type Lock struct {
	Message   string
	CreatedBy Actor
	CreatedAt time.Time
}

func readLock(fs billy.Filesystem, lockDir string) (*Lock, error) {
	lock := &Lock{}

	if cnt, err := readFile(fs, fs.Join(lockDir, "message")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		lock.Message = string(cnt)
	}

	if cnt, err := readFile(fs, fs.Join(lockDir, "created_by_email")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		lock.CreatedBy.Email = string(cnt)
	}

	if cnt, err := readFile(fs, fs.Join(lockDir, "created_by_name")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		lock.CreatedBy.Name = string(cnt)
	}

	if cnt, err := readFile(fs, fs.Join(lockDir, "created_at")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(string(cnt))); err != nil {
			return nil, err
		} else {
			lock.CreatedAt = createdAt
		}
	}

	return lock, nil
}

func (s *State) GetEnvironmentLocks(environment string) (map[string]Lock, error) {
	base := s.Filesystem.Join("environments", environment, "locks")
	if entries, err := s.Filesystem.ReadDir(base); err != nil {
		return nil, err
	} else {
		result := make(map[string]Lock, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				return nil, fmt.Errorf("error getting environment locks: found file in the locks directory. run migration script to generate correct metadata")
			}
			if lock, err := readLock(s.Filesystem, s.Filesystem.Join(base, e.Name())); err != nil {
				return nil, err
			} else {
				result[e.Name()] = *lock
			}
		}
		return result, nil
	}
}

func (s *State) GetEnvironmentApplicationLocks(environment, application string) (map[string]Lock, error) {
	base := s.Filesystem.Join("environments", environment, "applications", application, "locks")
	if entries, err := s.Filesystem.ReadDir(base); err != nil {
		return nil, err
	} else {
		result := make(map[string]Lock, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				return nil, fmt.Errorf("error getting application locks: found file in the locks directory. run migration script to generate correct metadata")
			}
			if lock, err := readLock(s.Filesystem, s.Filesystem.Join(base, e.Name())); err != nil {
				return nil, err
			} else {
				result[e.Name()] = *lock
			}
		}
		return result, nil
	}
}

func (s *State) GetDeploymentMetaData(ctx context.Context, environment, application string) (string, time.Time, error) {
	base := s.Filesystem.Join("environments", environment, "applications", application)
	author, err := readFile(s.Filesystem, s.Filesystem.Join(base, "deployed_by"))
	if err != nil {
		if os.IsNotExist(err) {
			// for backwards compatibility, we do not return an error here
			return "", time.Time{}, nil
		} else {
			return "", time.Time{}, err
		}
	}

	time_utc, err := readFile(s.Filesystem, s.Filesystem.Join(base, "deployed_at_utc"))
	if err != nil {
		if os.IsNotExist(err) {
			return string(author), time.Time{}, nil
		} else {
			return "", time.Time{}, err
		}
	}

	deployedAt, err := time.Parse("2006-01-02 15:04:05 -0700 MST", strings.TrimSpace(string(time_utc)))
	if err != nil {
		return "", time.Time{}, err
	}

	return string(author), deployedAt, nil
}

func (s *State) GetQueuedVersion(environment string, application string) (*uint64, error) {
	return s.readSymlink(environment, application, queueFileName)
}

func (s *State) DeleteQueuedVersion(environment string, application string) error {
	queuedVersion := s.Filesystem.Join("environments", environment, "applications", application, queueFileName)
	return s.Filesystem.Remove(queuedVersion)
}

func (s *State) DeleteQueuedVersionIfExists(environment string, application string) error {
	queuedVersion, err := s.GetQueuedVersion(environment, application)
	if err != nil {
		return err
	}
	if queuedVersion == nil {
		return nil // nothing to do
	}
	return s.DeleteQueuedVersion(environment, application)
}

func (s *State) GetEnvironmentApplicationVersion(environment, application string) (*uint64, error) {
	return s.readSymlink(environment, application, "version")
}

// returns nil if there is no file
func (s *State) readSymlink(environment string, application string, symlinkName string) (*uint64, error) {
	version := s.Filesystem.Join("environments", environment, "applications", application, symlinkName)
	if lnk, err := s.Filesystem.Readlink(version); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// if the link does not exist, we return nil
			return nil, nil
		}
		return nil, fmt.Errorf("failed reading symlink %q: %w", version, err)
	} else {
		target := s.Filesystem.Join("environments", environment, "applications", application, lnk)
		if stat, err := s.Filesystem.Stat(target); err != nil {
			// if the file that the link points to does not exist, that's an error
			return nil, fmt.Errorf("failed stating %q: %w", target, err)
		} else {
			res, err := strconv.ParseUint(stat.Name(), 10, 64)
			return &res, err
		}
	}
}

var invalidJson = errors.New("JSON file is not valid")

func envExists(envConfigs map[string]config.EnvironmentConfig, envNameToSearchFor string) bool {
	if _, found := envConfigs[envNameToSearchFor]; found {
		return true
	}
	return false
}

func (s *State) GetEnvironmentConfigsAndValidate(ctx context.Context) (map[string]config.EnvironmentConfig, error) {
	logger := logger.FromContext(ctx)
	envConfigs, err := s.GetEnvironmentConfigs()
	if err != nil {
		return nil, err
	}
	if len(envConfigs) == 0 {
		logger.Warn("No environment configurations found. Check git settings like the branch name. Kuberpult cannot operate without environments.")
	}
	for envName, env := range envConfigs {
		if env.Upstream == nil || env.Upstream.Environment == "" {
			continue
		}
		upstreamEnv := env.Upstream.Environment
		if !envExists(envConfigs, upstreamEnv) {
			logger.Warn(fmt.Sprintf("The environment '%s' has upstream '%s' configured, but the environment '%s' does not exist.", envName, upstreamEnv, upstreamEnv))
		}
	}
	envGroups := mapper.MapEnvironmentsToGroups(envConfigs)
	for _, group := range envGroups {
		grpDist := group.Environments[0].DistanceToUpstream
		for _, env := range group.Environments {
			if env.DistanceToUpstream != grpDist {
				logger.Warn(fmt.Sprintf("The environment group '%s' has multiple environments setup with different distances to upstream", group.EnvironmentGroupName))
			}
		}
	}
	return envConfigs, err
}

func (s *State) GetEnvironmentConfigs() (map[string]config.EnvironmentConfig, error) {
	if s.BootstrapMode {
		result := map[string]config.EnvironmentConfig{}
		buf, err := ioutil.ReadFile(s.EnvironmentConfigsPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return result, nil
			}
			return nil, err
		}
		err = json.Unmarshal(buf, &result)
		if err != nil {
			return nil, err
		}
		return result, nil
	} else {
		envs, err := s.Filesystem.ReadDir("environments")
		if err != nil {
			return nil, err
		}
		result := map[string]config.EnvironmentConfig{}
		for _, env := range envs {
			fileName := s.Filesystem.Join("environments", env.Name(), "config.json")
			var config config.EnvironmentConfig
			if err := decodeJsonFile(s.Filesystem, fileName, &config); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					result[env.Name()] = config
				} else {
					return nil, fmt.Errorf("%s : %w", fileName, invalidJson)
				}
			} else {
				result[env.Name()] = config
			}
		}
		return result, nil
	}
}

func (s *State) GetEnvironmentConfigsForGroup(envGroup string) ([]string, error) {
	allEnvConfigs, err := s.GetEnvironmentConfigs()
	if err != nil {
		return nil, err
	}
	groupEnvNames := []string{}
	for env := range allEnvConfigs {
		envConfig := allEnvConfigs[env]
		g := envConfig.EnvironmentGroup
		if g != nil && *g == envGroup {
			groupEnvNames = append(groupEnvNames, env)
		}
	}
	if len(groupEnvNames) == 0 {
		return nil, errors.New(fmt.Sprintf("No environment found with given group '%s'", envGroup))
	}
	sort.Strings(groupEnvNames)
	return groupEnvNames, nil
}

func (s *State) GetEnvironmentApplications(environment string) ([]string, error) {
	appDir := s.Filesystem.Join("environments", environment, "applications")
	return names(s.Filesystem, appDir)
}

func (s *State) GetApplications() ([]string, error) {
	return names(s.Filesystem, "applications")
}

func (s *State) GetApplicationReleases(application string) ([]uint64, error) {
	if ns, err := names(s.Filesystem, s.Filesystem.Join("applications", application, "releases")); err != nil {
		return nil, err
	} else {
		result := make([]uint64, 0, len(ns))
		for _, n := range ns {
			if i, err := strconv.ParseUint(n, 10, 64); err == nil {
				result = append(result, i)
			}
		}
		sort.Slice(result, func(i, j int) bool {
			return result[i] < result[j]
		})
		return result, nil
	}
}

type Release struct {
	Version uint64
	/**
	"UndeployVersion=true" means that this version is empty, and has no manifest that could be deployed.
	It is intended to help cleanup old services within the normal release cycle (e.g. dev->staging->production).
	*/
	UndeployVersion bool
	SourceAuthor    string
	SourceCommitId  string
	SourceMessage   string
	CreatedAt       time.Time
	DisplayVersion  string
}

func (s *State) IsUndeployVersion(application string, version uint64) (bool, error) {
	base := releasesDirectoryWithVersion(s.Filesystem, application, version)
	_, err := s.Filesystem.Stat(base)
	if err != nil {
		return false, wrapFileError(err, base, "could not call stat")
	}
	if _, err := readFile(s.Filesystem, s.Filesystem.Join(base, "undeploy")); err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (s *State) GetApplicationRelease(application string, version uint64) (*Release, error) {
	base := releasesDirectoryWithVersion(s.Filesystem, application, version)
	_, err := s.Filesystem.Stat(base)
	if err != nil {
		return nil, wrapFileError(err, base, "could not call stat")
	}
	release := Release{Version: version}
	if cnt, err := readFile(s.Filesystem, s.Filesystem.Join(base, "source_commit_id")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		release.SourceCommitId = string(cnt)
	}
	if cnt, err := readFile(s.Filesystem, s.Filesystem.Join(base, "source_author")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		release.SourceAuthor = string(cnt)
	}
	if cnt, err := readFile(s.Filesystem, s.Filesystem.Join(base, "source_message")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		release.SourceMessage = string(cnt)
	}
	if displayVersion, err := readFile(s.Filesystem, s.Filesystem.Join(base, "display_version")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		release.DisplayVersion = ""
	} else {
		release.DisplayVersion = string(displayVersion)
	}
	isUndeploy, err := s.IsUndeployVersion(application, version)
	if err != nil {
		return nil, err
	}
	release.UndeployVersion = isUndeploy
	if cnt, err := readFile(s.Filesystem, s.Filesystem.Join(base, "created_at")); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if releaseTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(cnt))); err != nil {
			return nil, err
		} else {
			release.CreatedAt = releaseTime
		}
	}
	return &release, nil
}

func (s *State) GetApplicationTeamOwner(application string) (string, error) {
	appDir := applicationDirectory(s.Filesystem, application)
	appTeam := s.Filesystem.Join(appDir, "team")

	if team, err := readFile(s.Filesystem, appTeam); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		} else {
			return "", fmt.Errorf("error while reading team owner file for application %v found: %w", application, err)
		}
	} else {
		return string(team), nil
	}
}

func (s *State) GetApplicationSourceRepoUrl(application string) (string, error) {
	appDir := applicationDirectory(s.Filesystem, application)
	appSourceRepoUrl := s.Filesystem.Join(appDir, "sourceRepoUrl")

	if url, err := readFile(s.Filesystem, appSourceRepoUrl); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		} else {
			return "", fmt.Errorf("error while reading sourceRepoUrl file for application %v found: %w", application, err)
		}
	} else {
		return string(url), nil
	}
}

func names(fs billy.Filesystem, path string) ([]string, error) {
	files, err := fs.ReadDir(path)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(files))
	for _, app := range files {
		result = append(result, app.Name())
	}
	return result, nil
}

func decodeJsonFile(fs billy.Filesystem, path string, out interface{}) error {
	if file, err := fs.Open(path); err != nil {
		return wrapFileError(err, path, "could not decode json file")
	} else {
		defer file.Close()
		dec := json.NewDecoder(file)
		return dec.Decode(out)
	}
}

func readFile(fs billy.Filesystem, path string) ([]byte, error) {
	if file, err := fs.Open(path); err != nil {
		return nil, err
	} else {
		defer file.Close()
		return io.ReadAll(file)
	}
}

// ProcessQueue checks if there is something in the queue
// deploys if necessary
// deletes the queue
func (s *State) ProcessQueue(ctx context.Context, fs billy.Filesystem, environment string, application string) (string, error) {
	queuedVersion, err := s.GetQueuedVersion(environment, application)
	queueDeploymentMessage := ""
	if err != nil {
		// could not read queued version.
		return "", err
	} else {
		if queuedVersion == nil {
			// if there is no version queued, that's not an issue, just do nothing:
			return "", nil
		}

		currentlyDeployedVersion, err := s.GetEnvironmentApplicationVersion(environment, application)
		if err != nil {
			return "", err
		}

		if currentlyDeployedVersion != nil && *queuedVersion == *currentlyDeployedVersion {
			// delete queue, it's outdated! But if we can't, that's not really a problem, as it would be overwritten
			// whenever the next deployment happens:
			err = s.DeleteQueuedVersion(environment, application)
			return fmt.Sprintf("deleted queued version %d because it was already deployed. app=%q env=%q", *queuedVersion, application, environment), err
		}
	}
	return queueDeploymentMessage, nil
}
