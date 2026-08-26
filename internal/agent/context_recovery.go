package agent

import (
	"context"
	"fmt"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

type contextRecoveryBudget struct {
	outputRetries      int
	maintenanceRetries int
}

func (a *Agent) recoverContextLimit(ctx context.Context, frozen samplingRequest, err error, budget *contextRecoveryBudget) (samplingRequest, bool, string, error) {
	limit := provider.AsContextLimitError(err)
	if a == nil || limit == nil || budget == nil {
		return samplingRequest{}, false, contextRecoveryFailed, nil
	}
	omitted := frozen.req.MaxTokens == 0
	if limit.PromptTokens > 0 {
		a.setPromptTokenCalibrationFromActive(limit.PromptTokens)
	}
	a.learnContextBudget(limit.WindowTokens, limit.CompletionTokens, omitted)
	adm := a.lastAdmission()
	adm.ObservedWindow = limit.WindowTokens
	adm.ObservedPrompt = limit.PromptTokens
	adm.ObservedCompletion = limit.CompletionTokens
	a.storeAdmission(adm)

	window := a.effectiveContextWindow()
	prompt := limit.PromptTokens
	if prompt <= 0 {
		prompt = a.estimatedRequestTokens(frozen.req)
	}
	margin := protocolMarginForWindow(window)
	physical := window - prompt - margin
	if physical > 0 && budget.outputRetries == 0 {
		return a.retryWithLearnedOutput(frozen, limit, budget, adm, window, prompt, physical, omitted)
	}
	if physical <= 0 && budget.maintenanceRetries == 0 {
		return a.retryAfterContextMaintenance(ctx, frozen, limit, budget, window, prompt)
	}
	a.setLastRecovery(contextRecoveryFailed)
	terminal := &IrreducibleContextError{
		Reason: IrreducibleNoProjectionProgress, EffectiveWindow: window,
		EstimatedPrompt: prompt, ProjectionGen: a.maintenanceProgressSnapshot().Generation,
		InputHash: a.contextMaintenanceInputHash(frozen.req.Messages),
		Detail:    "context recovery exhausted its output and maintenance retry budgets",
	}
	return samplingRequest{}, false, contextRecoveryFailed, terminal
}

func (a *Agent) retryWithLearnedOutput(frozen samplingRequest, limit *provider.ContextLimitError, budget *contextRecoveryBudget, adm contextAdmission, window, prompt, physical int, omitted bool) (samplingRequest, bool, string, error) {
	next := freezeProviderRequest(frozen.req)
	next.MaxTokens = physical
	if frozen.req.MaxTokens > 0 && frozen.req.MaxTokens < physical {
		next.MaxTokens = frozen.req.MaxTokens
	}
	budget.outputRetries++
	adm.WindowMode = provider.ContextWindowShared.String()
	adm.Source = provider.ContextBudgetSourceLearned
	adm.WindowTokens = window
	adm.PromptTokens = prompt
	adm.PhysicalRemaining = physical
	if adm.RequestedOutputTokens <= 0 {
		adm.RequestedOutputTokens = limit.CompletionTokens
	}
	if omitted && adm.AutoOutputTokens <= 0 {
		adm.AutoOutputTokens = limit.CompletionTokens
	}
	adm.EffectiveOutputTokens = next.MaxTokens
	adm.Clipped = adm.RequestedOutputTokens > 0 && next.MaxTokens < adm.RequestedOutputTokens
	adm.ApplyMaxTokens = next.MaxTokens > 0
	adm.LastRecovery = contextRecoveryLearnedRetry
	a.storeAdmission(adm)
	a.emitContextRecoveryNotice(contextRecoveryLearnedRetry, limit, next.MaxTokens)
	shape := a.requestCalibrationShape(next)
	a.sess.output.activeReqShape.Store(&shape)
	return samplingRequest{req: next}, true, contextRecoveryLearnedRetry, nil
}

func (a *Agent) retryAfterContextMaintenance(ctx context.Context, frozen samplingRequest, limit *provider.ContextLimitError, budget *contextRecoveryBudget, window, prompt int) (samplingRequest, bool, string, error) {
	before := a.maintenanceProgressSnapshot()
	inputHash := a.contextMaintenanceInputHash(frozen.req.Messages)
	if blocked, reason := a.contextMaintenanceBlocked(inputHash); blocked {
		return a.failedMaintenanceRetry(window, prompt, before.Generation, inputHash,
			"maintenance is already blocked for this active turn: "+reason)
	}
	key := MaintenanceAttemptKey{
		InputHash: inputHash, ProjectionGen: before.Generation,
		EffectiveWindow: window, Trigger: CompactionTriggerOverflow,
	}
	if !a.registerMaintenanceAttempt(key) {
		return a.failedMaintenanceRetry(window, prompt, before.Generation, key.InputHash,
			"the same maintenance input/generation/window already failed")
	}
	if _, err := a.contextManager().Prepare(ctx, ContextPreparePolicy{
		Trigger: CompactionTriggerOverflow,
		Force:   true,
	}); err != nil {
		a.setLastRecovery(contextRecoveryFailed)
		return samplingRequest{}, false, contextRecoveryFailed, err
	}
	after := a.maintenanceProgressSnapshot()
	failingHash := providerVisibleFingerprint(modelInputMessages(frozen.req.Messages))
	if !maintenanceMadeProgress(before, after, failingHash) {
		return a.failedMaintenanceRetry(window, prompt, after.Generation, key.InputHash,
			"maintenance did not install a smaller, newer provider-visible projection")
	}
	rebuilt, err := a.buildSamplingRequest(ctx, CompactionTriggerPressure)
	if err != nil {
		a.setLastRecovery(contextRecoveryFailed)
		return samplingRequest{}, false, contextRecoveryFailed, err
	}
	if err := a.applyAdmissionToRequest(&rebuilt.req); err != nil {
		a.setLastRecovery(contextRecoveryFailed)
		return samplingRequest{}, false, contextRecoveryFailed, err
	}
	if providerVisibleFingerprint(modelInputMessages(rebuilt.req.Messages)) == failingHash {
		return a.failedMaintenanceRetry(window, prompt, after.Generation, key.InputHash,
			"rebuilt sampling request is byte-identical to the rejected request")
	}
	budget.maintenanceRetries++
	a.setLastRecovery(contextRecoveryCompacted)
	a.emitContextRecoveryNotice(contextRecoveryCompacted, limit, rebuilt.req.MaxTokens)
	shape := a.requestCalibrationShape(rebuilt.req)
	a.sess.output.activeReqShape.Store(&shape)
	return samplingRequest{req: freezeProviderRequest(rebuilt.req)}, true, contextRecoveryCompacted, nil
}

func (a *Agent) failedMaintenanceRetry(window, prompt int, generation uint64, inputHash, detail string) (samplingRequest, bool, string, error) {
	a.setLastRecovery(contextRecoveryFailed)
	terminal := &IrreducibleContextError{
		Reason: IrreducibleNoProjectionProgress, EffectiveWindow: window,
		EstimatedPrompt: prompt, ProjectionGen: generation, InputHash: inputHash, Detail: detail,
	}
	return samplingRequest{}, false, contextRecoveryFailed, terminal
}

func (a *Agent) registerMaintenanceAttempt(key MaintenanceAttemptKey) bool {
	if a == nil {
		return false
	}
	if a.turn.maintenanceAttempts == nil {
		a.turn.maintenanceAttempts = make(map[MaintenanceAttemptKey]struct{})
	}
	if _, exists := a.turn.maintenanceAttempts[key]; exists {
		return false
	}
	a.turn.maintenanceAttempts[key] = struct{}{}
	return true
}

func (a *Agent) emitContextRecoveryNotice(kind string, limit *provider.ContextLimitError, nextOutput int) {
	if a == nil || a.svc.sink == nil {
		return
	}
	text := "Adjusted the output budget to fit the shared context window."
	if kind == contextRecoveryCompacted {
		text = "Compacted context after a shared-window overflow and retried."
	}
	detail := fmt.Sprintf("recovery=%s next_output=%d", kind, nextOutput)
	if limit != nil {
		detail = fmt.Sprintf("%s window=%d prompt=%d completion=%d requested=%d",
			detail, limit.WindowTokens, limit.PromptTokens, limit.CompletionTokens, limit.RequestedTokens)
	}
	a.svc.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text, Detail: detail})
}
