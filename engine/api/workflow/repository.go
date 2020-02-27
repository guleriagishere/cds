package workflow

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsamin/go-dump"
	"github.com/go-gorp/gorp"
	"gopkg.in/yaml.v2"

	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/keys"
	"github.com/ovh/cds/engine/api/observability"
	"github.com/ovh/cds/engine/api/operation"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
	"github.com/ovh/cds/sdk/log"
)

// WorkflowAsCodePattern is the default code pattern to find cds files
const WorkflowAsCodePattern = ".cds/**/*.yml"

// PushOption is the set of options for workflow push
type PushOption struct {
	VCSServer          string
	FromRepository     string
	Branch             string
	IsDefaultBranch    bool
	RepositoryName     string
	RepositoryStrategy sdk.RepositoryStrategy
	HookUUID           string
	Force              bool
	OldWorkflow        *sdk.Workflow
}

// CreateFromRepository a workflow from a repository
func CreateFromRepository(ctx context.Context, db *gorp.DbMap, store cache.Store, p *sdk.Project, w *sdk.Workflow,
	opts sdk.WorkflowRunPostHandlerOption, u sdk.Identifiable, decryptFunc keys.DecryptFunc) ([]sdk.Message, error) {
	ctx, end := observability.Span(ctx, "workflow.CreateFromRepository")
	defer end()

	ope, err := createOperationRequest(*w, opts)
	if err != nil {
		return nil, sdk.WrapError(err, "unable to create operation request")
	}

	if err := operation.PostRepositoryOperation(ctx, db, *p, &ope, nil); err != nil {
		return nil, sdk.WrapError(err, "unable to post repository operation")
	}

	if err := pollRepositoryOperation(ctx, db, store, &ope); err != nil {
		return nil, sdk.WrapError(err, "cannot analyse repository")
	}

	var uuid string
	if opts.Hook != nil {
		uuid = opts.Hook.WorkflowNodeHookUUID
	} else {
		// Search for repo web hook uuid
		for _, h := range w.WorkflowData.Node.Hooks {
			if h.HookModelName == sdk.RepositoryWebHookModelName {
				uuid = h.UUID
				break
			}
		}
	}
	return extractWorkflow(ctx, db, store, p, w, ope, u, decryptFunc, uuid)
}

func extractWorkflow(ctx context.Context, db *gorp.DbMap, store cache.Store, p *sdk.Project, w *sdk.Workflow,
	ope sdk.Operation, ident sdk.Identifiable, decryptFunc keys.DecryptFunc, hookUUID string) ([]sdk.Message, error) {
	ctx, end := observability.Span(ctx, "workflow.extractWorkflow")
	defer end()
	var allMsgs []sdk.Message
	// Read files
	tr, err := ReadCDSFiles(ope.LoadFiles.Results)
	if err != nil {
		allMsgs = append(allMsgs, sdk.NewMessage(sdk.MsgWorkflowErrorBadCdsDir))
		return allMsgs, sdk.WrapError(err, "unable to read cds files")
	}
	ope.RepositoryStrategy.SSHKeyContent = ""
	opt := &PushOption{
		VCSServer:          ope.VCSServer,
		RepositoryName:     ope.RepoFullName,
		RepositoryStrategy: ope.RepositoryStrategy,
		Branch:             ope.Setup.Checkout.Branch,
		FromRepository:     ope.RepositoryInfo.FetchURL,
		IsDefaultBranch:    ope.Setup.Checkout.Tag == "" && ope.Setup.Checkout.Branch == ope.RepositoryInfo.DefaultBranch,
		HookUUID:           hookUUID,
		OldWorkflow:        w,
	}

	allMsg, workflowPushed, _, errP := Push(ctx, db, store, p, tr, opt, ident, decryptFunc)
	if errP != nil {
		return allMsg, sdk.WrapError(errP, "unable to get workflow from file")
	}

	if w.Name != workflowPushed.Name {
		log.Debug("workflow.extractWorkflow> Workflow has been renamed from %s to %s", w.Name, workflowPushed.Name)
	}
	*w = *workflowPushed

	return append(allMsgs, allMsg...), nil
}

// ReadCDSFiles reads CDS files
func ReadCDSFiles(files map[string][]byte) (*tar.Reader, error) {
	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)
	// Create a new tar archive.
	tw := tar.NewWriter(buf)
	// Add some files to the archive.
	for fname, fcontent := range files {
		log.Debug("ReadCDSFiles> Reading %s", fname)
		hdr := &tar.Header{
			Name: filepath.Base(fname),
			Mode: 0600,
			Size: int64(len(fcontent)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, sdk.WrapError(err, "Cannot write header")
		}
		if n, err := tw.Write(fcontent); err != nil {
			return nil, sdk.WrapError(err, "Cannot write content")
		} else if n == 0 {
			return nil, fmt.Errorf("nothing to write")
		}
	}
	// Make sure to check the error on Close.
	if err := tw.Close(); err != nil {
		return nil, err
	}

	return tar.NewReader(buf), nil
}

type exportedEntities struct {
	wrkflw exportentities.Workflow
	apps   map[string]exportentities.Application
	pips   map[string]exportentities.PipelineV1
	envs   map[string]exportentities.Environment
}

func extractFromCDSFiles(ctx context.Context, tr *tar.Reader) (*exportedEntities, error) {
	var res = exportedEntities{
		apps: make(map[string]exportentities.Application),
		pips: make(map[string]exportentities.PipelineV1),
		envs: make(map[string]exportentities.Environment),
	}

	mError := new(sdk.MultiError)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			err = sdk.NewError(sdk.ErrWrongRequest, fmt.Errorf("Unable to read tar file"))
			return nil, sdk.WithStack(err)
		}

		log.Debug("Push> Reading %s", hdr.Name)

		buff := new(bytes.Buffer)
		if _, err := io.Copy(buff, tr); err != nil {
			err = sdk.NewError(sdk.ErrWrongRequest, fmt.Errorf("Unable to read tar file"))
			return nil, sdk.WithStack(err)
		}

		var workflowFileName string
		b := buff.Bytes()
		switch {
		case strings.Contains(hdr.Name, ".app."):
			var app exportentities.Application
			if err := yaml.Unmarshal(b, &app); err != nil {
				log.Error(ctx, "Push> Unable to unmarshal application %s: %v", hdr.Name, err)
				mError.Append(fmt.Errorf("Unable to unmarshal application %s: %v", hdr.Name, err))
				continue
			}
			res.apps[hdr.Name] = app
		case strings.Contains(hdr.Name, ".pip."):
			var pip exportentities.PipelineV1
			if err := yaml.Unmarshal(b, &pip); err != nil {
				log.Error(ctx, "Push> Unable to unmarshal pipeline %s: %v", hdr.Name, err)
				mError.Append(fmt.Errorf("Unable to unmarshal pipeline %s: %v", hdr.Name, err))
				continue
			}
			res.pips[hdr.Name] = pip
		case strings.Contains(hdr.Name, ".env."):
			var env exportentities.Environment
			if err := yaml.Unmarshal(b, &env); err != nil {
				log.Error(ctx, "Push> Unable to unmarshal environment %s: %v", hdr.Name, err)
				mError.Append(fmt.Errorf("Unable to unmarshal environment %s: %v", hdr.Name, err))
				continue
			}
			res.envs[hdr.Name] = env
		default:
			// if a workflow was already found, it's a mistake
			if workflowFileName != "" {
				log.Error(ctx, "two workflows files found: %s and %s", workflowFileName, hdr.Name)
				mError.Append(fmt.Errorf("two workflows files found: %s and %s", workflowFileName, hdr.Name))
				break
			}
			if err := yaml.Unmarshal(b, &res.wrkflw); err != nil {
				log.Error(ctx, "Push> Unable to unmarshal workflow %s: %v", hdr.Name, err)
				mError.Append(fmt.Errorf("Unable to unmarshal workflow %s: %v", hdr.Name, err))
				continue
			}
		}
	}

	// We only use the multiError during unmarshalling steps.
	// When a DB transaction has been started, just return at the first error
	// because transaction may have to be aborted
	if !mError.IsEmpty() {
		return nil, sdk.NewError(sdk.ErrWorkflowInvalid, mError)
	}

	return &res, nil
}

func pollRepositoryOperation(c context.Context, db gorp.SqlExecutor, store cache.Store, ope *sdk.Operation) error {
	tickTimeout := time.NewTicker(10 * time.Minute)
	tickPoll := time.NewTicker(2 * time.Second)
	defer tickTimeout.Stop()
	for {
		select {
		case <-c.Done():
			if c.Err() != nil {
				return sdk.WrapError(c.Err(), "pollRepositoryOperation> Exiting")
			}
		case <-tickTimeout.C:
			return sdk.WrapError(sdk.ErrRepoOperationTimeout, "pollRepositoryOperation> Timeout analyzing repository")
		case <-tickPoll.C:
			if err := operation.GetRepositoryOperation(c, db, ope); err != nil {
				return sdk.WrapError(err, "Cannot get repository operation status")
			}
			switch ope.Status {
			case sdk.OperationStatusError:
				opeTrusted := *ope
				opeTrusted.RepositoryStrategy.SSHKeyContent = "***"
				opeTrusted.RepositoryStrategy.Password = "***"
				return sdk.WrapError(fmt.Errorf("%s", ope.Error), "getImportAsCodeHandler> Operation in error. %+v", opeTrusted)
			case sdk.OperationStatusDone:
				return nil
			}
			continue
		}
	}
}

func createOperationRequest(w sdk.Workflow, opts sdk.WorkflowRunPostHandlerOption) (sdk.Operation, error) {
	ope := sdk.Operation{}
	if w.WorkflowData.Node.Context.ApplicationID == 0 {
		return ope, sdk.WrapError(sdk.ErrApplicationNotFound, "CreateFromRepository> Workflow node root does not have a application context")
	}
	app := w.Applications[w.WorkflowData.Node.Context.ApplicationID]
	ope = sdk.Operation{
		VCSServer:          app.VCSServer,
		RepoFullName:       app.RepositoryFullname,
		URL:                w.FromRepository,
		RepositoryStrategy: app.RepositoryStrategy,
		Setup: sdk.OperationSetup{
			Checkout: sdk.OperationCheckout{
				Branch: "",
				Commit: "",
			},
		},
		LoadFiles: sdk.OperationLoadFiles{
			Pattern: WorkflowAsCodePattern,
		},
	}

	var branch, commit, tag string
	if opts.Hook != nil {
		tag = opts.Hook.Payload[tagGitTag]
		branch = opts.Hook.Payload[tagGitBranch]
		commit = opts.Hook.Payload[tagGitHash]
	}
	if opts.Manual != nil {
		e := dump.NewDefaultEncoder()
		e.Formatters = []dump.KeyFormatterFunc{dump.WithDefaultLowerCaseFormatter()}
		e.ExtraFields.DetailedMap = false
		e.ExtraFields.DetailedStruct = false
		e.ExtraFields.Len = false
		e.ExtraFields.Type = false
		m1, errm1 := e.ToStringMap(opts.Manual.Payload)
		if errm1 != nil {
			return ope, sdk.WrapError(errm1, "CreateFromRepository> Unable to compute payload")
		}
		tag = m1[tagGitTag]
		branch = m1[tagGitBranch]
		commit = m1[tagGitHash]
	}
	ope.Setup.Checkout.Tag = tag
	ope.Setup.Checkout.Commit = commit
	ope.Setup.Checkout.Branch = branch

	// This should not append because the hook must set a default payload with git.branch
	if ope.Setup.Checkout.Branch == "" && ope.Setup.Checkout.Tag == "" {
		return ope, sdk.WrapError(sdk.NewError(sdk.ErrWrongRequest, fmt.Errorf("branch or tag parameter are mandatories")), "createOperationRequest")
	}

	return ope, nil
}
