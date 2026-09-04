package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/plan"
)

const planUsage = "usage: chronos-code plan <status|list|show|graph|resume|pause|cancel|retry|events|verify-db|migrate|backup|restore|export|prune> --db <path> [flags]"

type planCommand struct {
	database, tenant, repository, task, planID, generation string
	node, lease, event, idempotencyKey, backup, source     string
	expectedVersion                                        int64
	maxAttempts                                            int
	yes, dryRun                                            bool
}

func runPlan() error {
	if len(os.Args) < 3 {
		return errors.New(planUsage)
	}
	operation := os.Args[2]
	command, err := parsePlanCommand(os.Args[3:])
	if err != nil {
		return err
	}
	if command.database == "" {
		return fmt.Errorf("plan: --db is required")
	}
	if planMutation(operation) && !command.yes && !command.dryRun {
		return fmt.Errorf("plan %s: --yes is required", operation)
	}
	if command.dryRun && operation != "prune" {
		return fmt.Errorf("plan %s: --dry-run is only supported by prune", operation)
	}

	ctx := context.Background()
	store, err := plan.OpenSQLStore(ctx, command.database)
	if err != nil {
		return fmt.Errorf("open plan store: %w", err)
	}
	defer store.Close()

	scope := plan.PlanScope{TenantID: plan.TenantID(command.tenant), RepositoryID: plan.RepositoryID(command.repository)}
	ref := plan.PlanRef{TaskID: plan.TaskID(command.task), PlanID: plan.PlanID(command.planID), Generation: plan.GenerationID(command.generation)}
	switch operation {
	case "status", "verify-db":
		result, err := store.Integrity(ctx)
		return writePlanResult(result, err)
	case "migrate":
		return writePlanResult(struct {
			Migrated bool `json:"migrated"`
		}{Migrated: true}, nil)
	case "list":
		result, err := store.List(ctx, scope)
		return writePlanResult(result, err)
	case "show":
		result, err := store.Show(ctx, scope, ref)
		return writePlanResult(result, err)
	case "graph":
		result, err := store.Graph(ctx, scope, ref)
		return writePlanResult(result, err)
	case "events":
		result, err := store.Events(ctx, scope, ref)
		return writePlanResult(result, err)
	case "resume":
		result, err := store.Resume(ctx, planControlRequest(scope, ref, command.expectedVersion))
		return writePlanResult(result, err)
	case "pause":
		result, err := store.Pause(ctx, planControlRequest(scope, ref, command.expectedVersion))
		return writePlanResult(result, err)
	case "cancel":
		result, err := store.Cancel(ctx, planControlRequest(scope, ref, command.expectedVersion))
		return writePlanResult(result, err)
	case "retry":
		err := store.Retry(ctx, plan.RetryRequest{Scope: scope, Ref: ref, NodeID: plan.NodeID(command.node), LeaseID: plan.LeaseID(command.lease), EventID: plan.EventID(command.event), IdempotencyKey: plan.IdempotencyKey(command.idempotencyKey), MaxAttempts: command.maxAttempts})
		return writePlanResult(struct {
			Retried bool `json:"retried"`
		}{Retried: err == nil}, err)
	case "backup":
		result, err := store.Backup(ctx, plan.BackupRequest{Path: command.backup})
		return writePlanResult(result, err)
	case "restore":
		result, err := store.Restore(ctx, plan.RestoreRequest{SourcePath: command.source, BackupPath: command.backup})
		return writePlanResult(result, err)
	case "export":
		result, err := store.Export(ctx, scope)
		return writePlanResult(result, err)
	case "prune":
		result, err := store.Prune(ctx, plan.PruneRequest{Scope: scope, DryRun: command.dryRun})
		return writePlanResult(result, err)
	default:
		return fmt.Errorf("unknown plan command: %s; %s", operation, planUsage)
	}
}

func planMutation(operation string) bool {
	switch operation {
	case "resume", "pause", "cancel", "retry", "restore", "prune":
		return true
	}
	return false
}

func planControlRequest(scope plan.PlanScope, ref plan.PlanRef, expectedVersion int64) plan.PlanControlRequest {
	return plan.PlanControlRequest{Scope: scope, Ref: ref, ExpectedVersion: expectedVersion}
}

func writePlanResult(value any, err error) error {
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		return fmt.Errorf("write plan result: %w", err)
	}
	return nil
}

func parsePlanCommand(args []string) (planCommand, error) {
	var command planCommand
	values := map[string]*string{
		"db": &command.database, "tenant": &command.tenant, "repository": &command.repository,
		"task": &command.task, "plan": &command.planID, "generation": &command.generation,
		"node": &command.node, "lease": &command.lease, "event": &command.event,
		"idempotency-key": &command.idempotencyKey, "backup": &command.backup, "source": &command.source,
	}
	seen := make(map[string]bool, len(args))
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "--") {
			return planCommand{}, fmt.Errorf("plan: unexpected argument %q", argument)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if name == "yes" || name == "dry-run" {
			if hasValue || seen[name] {
				return planCommand{}, fmt.Errorf("plan: invalid --%s", name)
			}
			seen[name] = true
			if name == "yes" {
				command.yes = true
			} else {
				command.dryRun = true
			}
			continue
		}
		if _, isValueFlag := values[name]; !isValueFlag && name != "expected-version" && name != "max-attempts" {
			return planCommand{}, fmt.Errorf("plan: unknown flag --%s", name)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return planCommand{}, fmt.Errorf("plan: --%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if seen[name] {
			return planCommand{}, fmt.Errorf("plan: --%s was specified more than once", name)
		}
		seen[name] = true
		if destination, ok := values[name]; ok {
			if value == "" {
				return planCommand{}, fmt.Errorf("plan: --%s requires a value", name)
			}
			*destination = value
			continue
		}
		switch name {
		case "expected-version":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 {
				return planCommand{}, fmt.Errorf("plan: --expected-version must be a positive integer")
			}
			command.expectedVersion = parsed
		case "max-attempts":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 {
				return planCommand{}, fmt.Errorf("plan: --max-attempts must be a positive integer")
			}
			command.maxAttempts = parsed
		default:
			return planCommand{}, fmt.Errorf("plan: unknown flag --%s", name)
		}
	}
	return command, nil
}
