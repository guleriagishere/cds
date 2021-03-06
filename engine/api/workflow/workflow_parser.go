package workflow

import (
	"context"
	"sync"

	"github.com/go-gorp/gorp"

	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/observability"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
	"github.com/ovh/cds/sdk/log"
)

// ImportOptions is option to parse a workflow
type ImportOptions struct {
	Force              bool
	WorkflowName       string
	FromRepository     string
	IsDefaultBranch    bool
	FromBranch         string
	VCSServer          string
	RepositoryName     string
	RepositoryStrategy sdk.RepositoryStrategy
	HookUUID           string
}

// Parse parse an exportentities.workflow and return the parsed workflow
func Parse(ctx context.Context, proj *sdk.Project, ew *exportentities.Workflow) (*sdk.Workflow, error) {
	log.Info(ctx, "Parse>> Parse workflow %s in project %s", ew.Name, proj.Key)
	log.Debug("Parse>> Workflow: %+v", ew)

	//Check valid application name
	rx := sdk.NamePatternRegex
	if !rx.MatchString(ew.Name) {
		return nil, sdk.WrapError(sdk.ErrInvalidApplicationPattern, "Workflow name %s do not respect pattern %s", ew.Name, sdk.NamePattern)
	}

	//Inherit permissions from project
	if len(ew.Permissions) == 0 {
		ew.Permissions = make(map[string]int)
		for _, p := range proj.ProjectGroups {
			ew.Permissions[p.Group.Name] = p.Permission
		}
	}

	//Parse workflow
	w, errW := ew.GetWorkflow()
	if errW != nil {
		return nil, sdk.NewError(sdk.ErrWrongRequest, errW)
	}
	w.ProjectID = proj.ID
	w.ProjectKey = proj.Key

	return w, nil
}

// ParseAndImport parse an exportentities.workflow and insert or update the workflow in database
func ParseAndImport(ctx context.Context, db gorp.SqlExecutor, store cache.Store, proj *sdk.Project, oldW *sdk.Workflow, ew *exportentities.Workflow, u sdk.Identifiable, opts ImportOptions) (*sdk.Workflow, []sdk.Message, error) {
	ctx, end := observability.Span(ctx, "workflow.ParseAndImport")
	defer end()

	log.Info(ctx, "ParseAndImport>> Import workflow %s in project %s (force=%v)", ew.Name, proj.Key, opts.Force)

	//Parse workflow
	w, errW := Parse(ctx, proj, ew)
	if errW != nil {
		return nil, nil, errW
	}

	// Load deep pipelines if we come from workflow run ( so we have hook uuid ).
	// We need deep pipelines to be able to run stages/jobs
	if err := IsValid(ctx, store, db, w, proj, LoadOptions{DeepPipeline: opts.HookUUID != ""}); err != nil {
		// Get spawn infos from error
		msg, ok := sdk.ErrorToMessage(err)
		if ok {
			return nil, []sdk.Message{msg}, sdk.WrapError(err, "Workflow is not valid")
		}
		return nil, nil, sdk.WrapError(err, "Workflow is not valid")
	}

	if err := RenameNode(ctx, db, w); err != nil {
		return nil, nil, sdk.WrapError(err, "Unable to rename node")
	}

	w.FromRepository = opts.FromRepository
	if !opts.IsDefaultBranch {
		w.DerivationBranch = opts.FromBranch
	}

	// do not override application data if no opts were given
	appID := w.WorkflowData.Node.Context.ApplicationID
	if opts.VCSServer != "" && appID != 0 {
		app := w.GetApplication(appID)
		app.VCSServer = opts.VCSServer
		app.RepositoryFullname = opts.RepositoryName
		app.RepositoryStrategy = opts.RepositoryStrategy
		w.Applications[appID] = app
	}

	if w.FromRepository != "" {
		// Get repowebhook from previous version of workflow
		var oldRepoWebHook *sdk.NodeHook
		if oldW != nil {
			for i := range oldW.WorkflowData.Node.Hooks {
				h := &oldW.WorkflowData.Node.Hooks[i]
				if h.HookModelName == sdk.RepositoryWebHookModel.Name {
					oldRepoWebHook = h
					break
				}
			}

			if oldRepoWebHook != nil {
				// Update current repo web hook if found
				var currentRepoWebHook *sdk.NodeHook
				// Get current webhook
				for i := range w.WorkflowData.Node.Hooks {
					h := &w.WorkflowData.Node.Hooks[i]
					if h.HookModelName == sdk.RepositoryWebHookModel.Name {
						h.UUID = oldRepoWebHook.UUID
						h.Config = oldRepoWebHook.Config.Clone()
						h.Config[sdk.HookConfigWorkflow] = sdk.WorkflowNodeHookConfigValue{Value: w.Name}
						currentRepoWebHook = h
						break
					}
				}

				// If not found
				if currentRepoWebHook == nil {
					h := sdk.NodeHook{
						UUID:          oldRepoWebHook.UUID,
						HookModelName: oldRepoWebHook.HookModelName,
						Config:        oldRepoWebHook.Config.Clone(),
						HookModelID:   oldRepoWebHook.HookModelID,
					}
					h.Config[sdk.HookConfigWorkflow] = sdk.WorkflowNodeHookConfigValue{Value: w.Name}
					w.WorkflowData.Node.Hooks = append(w.WorkflowData.Node.Hooks, h)
				}
			}
		}

		// If there is no old workflow OR workflow existing on CDS does not have a repoWebhook,
		// we have to create a new repo webhook
		if oldW == nil || oldRepoWebHook == nil {
			// Init new repo webhook
			var newRepoWebHook = sdk.NodeHook{
				HookModelName: sdk.RepositoryWebHookModel.Name,
				HookModelID:   sdk.RepositoryWebHookModel.ID,
				Config:        sdk.RepositoryWebHookModel.DefaultConfig.Clone(),
			}

			// If the new workflow already contains a repowebhook (comparing refs), we dont have to add a new one
			var hasARepoWebHook bool
			for _, h := range w.WorkflowData.Node.Hooks {
				if h.Ref() == newRepoWebHook.Ref() {
					hasARepoWebHook = true
					break
				}
			}
			if !hasARepoWebHook {
				w.WorkflowData.Node.Hooks = append(w.WorkflowData.Node.Hooks, newRepoWebHook)
			}

			var err error
			if w.WorkflowData.Node.Context.DefaultPayload, err = DefaultPayload(ctx, db, store, proj, w); err != nil {
				return nil, nil, sdk.WrapError(err, "Unable to get default payload")
			}
		}
	}

	if opts.WorkflowName != "" && w.Name != opts.WorkflowName {
		return nil, nil, sdk.WrapError(sdk.ErrWorkflowNameImport, "Wrong workflow name")
	}

	//Import
	done := new(sync.WaitGroup)
	done.Add(1)
	msgChan := make(chan sdk.Message)
	msgList := []sdk.Message{}
	go func(array *[]sdk.Message) {
		defer done.Done()
		for {
			m, more := <-msgChan
			if !more {
				return
			}
			*array = append(*array, m)
		}
	}(&msgList)

	globalError := Import(ctx, db, store, proj, oldW, w, u, opts.Force, msgChan)
	close(msgChan)
	done.Wait()

	return w, msgList, globalError
}
