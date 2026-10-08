package verify

import (
	"errors"
	"fmt"
	"time"

	"shephrd/internal/delivery"
	"shephrd/internal/lifecycle"
	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

type Boundary struct {
	Store                            *store.Store
	Verifier                         delivery.Verifier
	FinalizeEndpoint                 func(model.Attempt) error
	Release                          func(string, string) error
	WorkerContext                    func() bool
	ValidateReportRecoveryWorkspace  func(model.ReportRecoveryCandidate) error
	ReportRecoveryEndpointGone       func(model.Attempt) (bool, error)
	ReportAccepted                   *lifecycle.Dispatcher
	PresentReport                    func(model.VerifiedArtifact)
	PresentReportVerificationFailure func(model.Message)
}

type proofRecorder func(*delivery.Result) error

func (b Boundary) Remote(taskID, attemptID, driverID string) (delivery.Result, error) {
	task, err := b.Store.Task(taskID)
	if err != nil {
		return delivery.Result{}, err
	}
	if task.ArtifactRef != "" {
		if err := model.ValidateDeliverableArtifact(task.Deliverable, task.ArtifactRef); err != nil {
			return delivery.Result{}, err
		}
	}
	explicitAttempt := attemptID != ""
	if attemptID == "" {
		attemptID = task.CurrentAttemptID
	}
	attempt, err := b.Store.Attempt(attemptID)
	if err != nil {
		return delivery.Result{}, err
	}
	if attempt.TaskID != task.ID {
		return delivery.Result{}, fmt.Errorf("attempt %s does not belong to task %s", attempt.ID, task.ID)
	}
	attestation, err := b.Store.ExternalDeliveryAttestationForAttempt(attempt.ID)
	if err != nil {
		return delivery.Result{}, err
	}
	if attestation != nil {
		return b.verifyAttestedDelivery(task, attempt, *attestation, driverID, explicitAttempt)
	}
	reportRecovery, err := b.Store.ReportRecoveryAttestationForAttempt(attempt.ID)
	if err != nil {
		return delivery.Result{}, err
	}
	if reportRecovery != nil {
		return b.verifyRecoveredReport(task, attempt, *reportRecovery, driverID)
	}
	if explicitAttempt && attempt.ID != task.CurrentAttemptID {
		return delivery.Result{}, model.Failure("attempt_not_eligible", "remote verification of superseded attempt %s requires an external delivery attestation", attempt.ID)
	}
	if !task.ClaimedDone {
		if task.Deliverable == "report" {
			if command, commandErr := b.Store.ReportRecoveryCommand(task.ID, driverID); commandErr == nil {
				return delivery.Result{}, model.EvidenceFailure("report_recovery_attestation_required", map[string]string{
					"recovery_command": command,
					"inspect_command":  "shephrd task inspect " + task.ID + " --json",
				}, "task %s has no accepted worker done event; explicit report recovery attestation is required before verification", task.ID)
			}
		}
		return delivery.Result{}, fmt.Errorf("task %s has not claimed done; landed proof is checked only after a done event", task.ID)
	}
	if process.Alive(attempt.RunnerPID) {
		return delivery.Result{}, fmt.Errorf("attempt %s is still running; verify after the worker exits", attempt.ID)
	}
	if err := b.FinalizeEndpoint(attempt); err != nil {
		return delivery.Result{}, err
	}
	if proof, proofErr := b.Store.LandingProof(attempt.ID); proofErr == nil {
		return b.completeProof(task, attempt, resultFromProof(task, attempt, proof), nil, true)
	}
	result, record, err := b.deliveryEvidence(task, attempt)
	if err != nil {
		return result, errors.Join(err, b.recordReportVerificationFailure(task, attempt, reportVerificationFailureKind(err)))
	}
	if result.Landed {
		return b.completeProof(task, attempt, result, record, true)
	}
	if err := b.recordReportVerificationFailure(task, attempt, "report_not_verified"); err != nil {
		return result, err
	}
	return b.withReleaseState(result, attempt.ID)
}

func (b Boundary) Local(taskID, attemptID, driverID string) (delivery.Result, error) {
	task, err := b.Store.Task(taskID)
	if err != nil {
		return delivery.Result{}, err
	}
	if task.ArtifactRef != "" {
		if err := model.ValidateDeliverableArtifact(task.Deliverable, task.ArtifactRef); err != nil {
			return delivery.Result{}, err
		}
	}
	if attemptID == "" {
		attemptID = task.CurrentAttemptID
	}
	attempt, err := b.Store.Attempt(attemptID)
	if err != nil {
		return delivery.Result{}, err
	}
	if attempt.TaskID != task.ID {
		return delivery.Result{}, fmt.Errorf("attempt %s does not belong to task %s", attempt.ID, task.ID)
	}
	done, err := b.Store.AcceptedDone(attempt.ID, attempt.RunGeneration)
	if err != nil {
		return delivery.Result{}, err
	}
	if process.Alive(attempt.RunnerPID) {
		return delivery.Result{}, fmt.Errorf("attempt %s is still running; verify after the worker exits", attempt.ID)
	}
	if err := b.FinalizeEndpoint(attempt); err != nil {
		return delivery.Result{}, err
	}
	if proof, proofErr := b.Store.LandingProof(attempt.ID); proofErr == nil {
		if proof.Kind != "local_default_branch" && proof.Kind != model.LandingKindLocalAttestedAncestry {
			return delivery.Result{}, fmt.Errorf("attempt %s already has immutable %s landing proof", attempt.ID, proof.Kind)
		}
		return b.completeProof(task, attempt, resultFromProof(task, attempt, proof), nil, true)
	}
	checkpoint, err := b.Store.LatestCheckpoint(attempt.ID)
	if err != nil {
		return delivery.Result{}, err
	}
	repo, err := b.Store.Repo(task.RepoID)
	if err != nil {
		return delivery.Result{}, err
	}
	result, err := b.Verifier.VerifyLocal(task, repo, attempt, checkpoint, done.ArtifactRef)
	if err != nil || !result.Landed {
		return result, err
	}
	return b.completeProof(task, attempt, result, b.landingProofRecorder(task, attempt, result.Reason), true)
}

func (b Boundary) VerifyDelivery(task model.Task, attempt model.Attempt) (delivery.Result, error) {
	if err := model.ValidateDeliverableArtifact(task.Deliverable, task.ArtifactRef); err != nil {
		return delivery.Result{}, err
	}
	result, record, err := b.deliveryEvidence(task, attempt)
	if err != nil {
		return result, errors.Join(err, b.recordReportVerificationFailure(task, attempt, reportVerificationFailureKind(err)))
	}
	if !result.Landed {
		return result, b.recordReportVerificationFailure(task, attempt, "report_not_verified")
	}
	return b.completeProof(task, attempt, result, record, false)
}

func (b Boundary) verifyRecoveredReport(task model.Task, attempt model.Attempt, attestation model.ReportRecoveryAttestation, driverID string) (delivery.Result, error) {
	if b.WorkerContext() {
		return delivery.Result{}, model.Failure("worker_context_forbidden", "driver-attested report recovery verification cannot run in worker context")
	}
	candidate, err := b.Store.ReportRecoveryCandidateFor(task.ID, attempt.ID, driverID, attestation.RunGeneration,
		attestation.CheckpointRevision, attestation.CheckpointSourceCursor)
	if err != nil {
		return delivery.Result{}, err
	}
	if process.Alive(attempt.RunnerPID) {
		return delivery.Result{}, model.Failure("worker_process_alive", "attempt %s worker process is still alive", attempt.ID)
	}
	if b.ValidateReportRecoveryWorkspace == nil || b.ReportRecoveryEndpointGone == nil {
		return delivery.Result{}, model.Failure("report_recovery_unavailable", "report recovery workspace or terminal verifier is unavailable")
	}
	if err := b.ValidateReportRecoveryWorkspace(candidate); err != nil {
		return delivery.Result{}, err
	}
	if attempt.ReleaseState != model.WorkspaceStateReleased {
		gone, err := b.ReportRecoveryEndpointGone(attempt)
		if err != nil {
			return delivery.Result{}, err
		}
		if !gone {
			return delivery.Result{}, recoveredReportVerificationFailure(task, attempt, "terminal_endpoint_alive", "attempt %s exact terminal endpoint is still present", attempt.ID)
		}
	}
	file, err := b.Verifier.ValidateReportRecovery(task.ID, attempt)
	if err != nil {
		return delivery.Result{}, recoveredReportVerificationFailure(task, attempt, "report_file_invalid", "%v", err)
	}
	if !delivery.SameReportFileEvidence(attestation, file) {
		return delivery.Result{}, recoveredReportVerificationFailure(task, attempt, "attestation_conflict", "canonical report file no longer matches immutable recovery %s", attestation.ID)
	}
	recoveredTask := task
	recoveredTask.ArtifactRef = "report:" + attestation.CanonicalReportPath
	artifactResult, err := b.Verifier.Verify(recoveredTask, candidate.Repo, candidate.Attempt, candidate.Checkpoint)
	if err != nil {
		return artifactResult, err
	}
	if !artifactResult.Landed || artifactResult.VerifiedArtifact == nil || artifactResult.VerifiedArtifact.SHA256 != attestation.SHA256 || artifactResult.VerifiedArtifact.SizeBytes != attestation.SizeBytes {
		return artifactResult, model.Failure("attestation_conflict", "canonical report snapshot no longer matches immutable recovery %s", attestation.ID)
	}
	reason := fmt.Sprintf("driver-attested report recovery verified from canonical file %s with SHA-256 %s (%d bytes); no worker done event was synthesized", attestation.CanonicalReportPath, attestation.SHA256, attestation.SizeBytes)
	stored, err := b.Store.SetVerifiedRecoveredReportDelivery(candidate, attestation, *artifactResult.VerifiedArtifact, reason, b.reportAcceptedBindings()...)
	if err != nil {
		return delivery.Result{}, err
	}
	result := delivery.Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done",
		CompletionProvenance: model.CompletionProvenanceReportRecovery, Landed: true, Reason: reason,
		Landing:        delivery.LandingResult{Landed: true, Kind: "report_artifact", CheckpointRevision: attestation.CheckpointRevision, VerifiedAt: stored.VerifiedAt},
		RemoteDelivery: delivery.RemoteDeliveryResult{State: "unverified"}, VerifiedArtifact: &stored}
	if err := b.dispatchReportAccepted(&result); err != nil {
		return result, err
	}
	if err := b.Release(task.ID, attempt.ID); err != nil {
		return result, model.EvidenceFailure("workspace_release_pending", map[string]string{
			"release_command": fmt.Sprintf("shephrd workspace release %s --attempt %s --json", task.ID, attempt.ID),
			"inspect_command": "shephrd task inspect " + task.ID + " --json",
		}, "%v", err)
	}
	return b.withReleaseState(result, attempt.ID)
}

func recoveredReportVerificationFailure(task model.Task, attempt model.Attempt, kind, format string, args ...any) error {
	return model.EvidenceFailure(kind, map[string]string{
		"verification_command": fmt.Sprintf("shephrd task verify-delivery %s --attempt %s --driver-id %s --json", task.ID, attempt.ID, model.ShellQuote(task.DriverID)),
		"inspect_command":      "shephrd task inspect " + task.ID + " --json",
	}, format, args...)
}

func (b Boundary) verifyAttestedDelivery(task model.Task, attempt model.Attempt, attestation model.ExternalDeliveryAttestation, driverID string, explicitAttempt bool) (delivery.Result, error) {
	if b.WorkerContext() {
		return delivery.Result{}, model.Failure("worker_context_forbidden", "driver-attested delivery verification cannot run in worker context")
	}
	candidate, err := b.Store.DeliveryAttestationCandidate(task.ID, attempt.ID, driverID, attestation.SealedCommit, explicitAttempt)
	if err != nil {
		return delivery.Result{}, err
	}
	if proof, proofErr := b.Store.LandingProof(attempt.ID); proofErr == nil {
		if proof.Kind != "github_pr_attested_ancestry" || proof.SourceCommit != attestation.SealedCommit || proof.TargetRef != "refs/heads/"+attestation.RegisteredDefaultBranch || proof.TargetCommit != attestation.MergeCommit || proof.CheckpointRevision != attestation.CheckpointRevision {
			return delivery.Result{}, model.Failure("landing_proof_conflict", "attempt %s already has conflicting immutable %s landing proof", attempt.ID, proof.Kind)
		}
		return b.completeProof(task, attempt, resultFromProof(task, attempt, proof), nil, true)
	}
	evidence, err := b.Verifier.ValidateExternalDelivery(candidate.Repo, attestation.PRURL, attestation.SealedCommit)
	if err != nil {
		return delivery.Result{}, err
	}
	if !sameExternalEvidence(attestation, evidence) {
		return delivery.Result{}, model.Failure("attestation_conflict", "current GitHub evidence no longer matches immutable attestation %s", attestation.ID)
	}
	verifiedAt := time.Now().UTC()
	reason := fmt.Sprintf("driver-attested PR ancestry verified for sealed commit %s through %s into refs/heads/%s; original artifact %s was unchanged", attestation.SealedCommit, attestation.PRURL, attestation.RegisteredDefaultBranch, attestation.OriginalArtifactRef)
	result := delivery.Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done", CompletionProvenance: model.CompletionProvenanceWorkerDone, PRState: "MERGED", Landed: true, Reason: reason,
		Landing: delivery.LandingResult{Landed: true, Kind: "github_pr_attested_ancestry", SourceCommit: attestation.SealedCommit,
			TargetRef: "refs/heads/" + attestation.RegisteredDefaultBranch, TargetCommit: attestation.MergeCommit,
			CheckpointRevision: attestation.CheckpointRevision, VerifiedAt: verifiedAt},
		RemoteDelivery: delivery.RemoteDeliveryResult{State: "externally_attested", PRState: "MERGED", PRURL: attestation.PRURL, MergedAt: attestation.MergedAt}}
	return b.completeProof(task, attempt, result, b.landingProofRecorder(task, attempt, reason), true)
}

func (b Boundary) deliveryEvidence(task model.Task, attempt model.Attempt) (delivery.Result, proofRecorder, error) {
	attempt, err := b.Store.Attempt(attempt.ID)
	if err != nil {
		return delivery.Result{}, nil, err
	}
	repo, err := b.Store.Repo(task.RepoID)
	if err != nil {
		return delivery.Result{}, nil, err
	}
	checkpoint, checkpointErr := b.Store.LatestCheckpoint(attempt.ID)
	isReport := task.Deliverable == "report"
	if checkpointErr != nil && !isReport {
		return delivery.Result{}, nil, checkpointErr
	}
	if isReport {
		return b.reportEvidence(task, attempt, repo, checkpoint)
	}
	result, err := b.Verifier.Verify(task, repo, attempt, checkpoint)
	if err != nil {
		return result, nil, err
	}
	if err := b.Store.SetRemoteDelivery(task.ID, attempt.ID, task.CurrentAttemptID, attempt.RunGeneration,
		result.BranchPushed, result.RemoteDelivery.State, result.PRState); err != nil {
		return result, nil, err
	}
	if !result.Landed {
		return result, nil, nil
	}
	return result, b.landingProofRecorder(task, attempt, result.Reason), nil
}

func (b Boundary) reportEvidence(task model.Task, attempt model.Attempt, repo model.Repo, checkpoint model.AttemptCheckpoint) (delivery.Result, proofRecorder, error) {
	message, err := b.Store.AcceptedDoneMessage(task.ID, attempt.ID)
	if err != nil {
		return delivery.Result{}, nil, err
	}
	existing, err := b.Store.VerifiedArtifactForDoneMessage(message.ID)
	if err != nil {
		return delivery.Result{}, nil, err
	}
	var result delivery.Result
	if existing != nil {
		if err := b.Verifier.ValidateSnapshot(*existing); err != nil {
			return delivery.Result{}, nil, fmt.Errorf("verified report snapshot for producer task %s is invalid: %w", task.ID, err)
		}
		result = delivery.Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done", CompletionProvenance: model.CompletionProvenanceWorkerDone, Landed: true,
			Reason:         fmt.Sprintf("report artifact snapshot verified with SHA-256 %s (%d bytes)", existing.SHA256, existing.SizeBytes),
			Landing:        delivery.LandingResult{Landed: true, Kind: "report_artifact", VerifiedAt: existing.VerifiedAt},
			RemoteDelivery: delivery.RemoteDeliveryResult{State: "unverified"}, VerifiedArtifact: existing}
	} else {
		result, err = b.Verifier.Verify(task, repo, attempt, checkpoint)
		if err != nil {
			return result, nil, err
		}
	}
	if !result.Landed {
		if err := b.Store.SetRemoteDelivery(task.ID, attempt.ID, task.CurrentAttemptID, attempt.RunGeneration, false, "not_pushed", ""); err != nil {
			return result, nil, err
		}
		return result, nil, nil
	}
	if result.VerifiedArtifact == nil {
		return result, nil, fmt.Errorf("report verification for task %s did not produce immutable artifact metadata", task.ID)
	}
	return result, func(result *delivery.Result) error {
		stored, err := b.Store.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, *result.VerifiedArtifact, result.Reason, b.reportAcceptedBindings()...)
		if err != nil {
			return err
		}
		result.Landing.VerifiedAt = stored.VerifiedAt
		result.VerifiedArtifact = &stored
		return nil
	}, nil
}

func (b Boundary) landingProofRecorder(task model.Task, attempt model.Attempt, reason string) proofRecorder {
	return func(result *delivery.Result) error {
		proof, err := b.Store.RecordLandingProof(task.CurrentAttemptID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID,
			RunGeneration: attempt.RunGeneration, Kind: result.Landing.Kind, SourceCommit: result.Landing.SourceCommit,
			TargetRef: result.Landing.TargetRef, TargetCommit: result.Landing.TargetCommit,
			CheckpointRevision: result.Landing.CheckpointRevision, VerifiedAt: result.Landing.VerifiedAt}, reason)
		if err != nil {
			return err
		}
		result.Landing.VerifiedAt = proof.VerifiedAt
		return nil
	}
}

func (b Boundary) completeProof(task model.Task, attempt model.Attempt, result delivery.Result, record proofRecorder, release bool) (delivery.Result, error) {
	if record != nil {
		if err := record(&result); err != nil {
			return result, err
		}
	}
	if result.Landing.Kind == "report_artifact" {
		if result.VerifiedArtifact == nil {
			artifact, err := b.Store.VerifiedReportForAttempt(task.ID, attempt.ID)
			if err != nil {
				return result, err
			}
			result.VerifiedArtifact = artifact
		}
		if err := b.dispatchReportAccepted(&result); err != nil {
			return result, err
		}
	}
	if !release {
		return result, nil
	}
	if err := b.Release(task.ID, attempt.ID); err != nil {
		return result, err
	}
	return b.withReleaseState(result, attempt.ID)
}

func (b Boundary) recordReportVerificationFailure(task model.Task, attempt model.Attempt, failureKind string) error {
	if task.Deliverable != "report" {
		return nil
	}
	artifact, err := b.Store.VerifiedReportForAttempt(task.ID, attempt.ID)
	if err != nil {
		return fmt.Errorf("inspect report acceptance before recording verification failure: %w", err)
	}
	if artifact != nil {
		return nil
	}
	message, err := b.Store.RecordReportVerificationFailure(task.ID, attempt.ID, failureKind)
	if err != nil {
		return fmt.Errorf("persist report verification failure presentation: %w", err)
	}
	if b.PresentReportVerificationFailure != nil {
		b.PresentReportVerificationFailure(message)
	}
	return nil
}

func reportVerificationFailureKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	var typed classified
	if errors.As(err, &typed) && typed.ErrorKind() != "" {
		return typed.ErrorKind()
	}
	return "report_verification_failed"
}

func (b Boundary) reportAcceptedBindings() []model.ReportAcceptedHandlerBinding {
	if b.ReportAccepted == nil {
		return nil
	}
	return b.ReportAccepted.Bindings()
}

func (b Boundary) dispatchReportAccepted(result *delivery.Result) error {
	if result.VerifiedArtifact == nil {
		return fmt.Errorf("accepted report has no immutable artifact identity")
	}
	if b.ReportAccepted != nil {
		invocations, err := b.ReportAccepted.Dispatch(*result.VerifiedArtifact)
		if err != nil {
			return err
		}
		result.LifecycleHandlers = invocations
	}
	if b.PresentReport != nil {
		b.PresentReport(*result.VerifiedArtifact)
	}
	return nil
}

func (b Boundary) withReleaseState(result delivery.Result, attemptID string) (delivery.Result, error) {
	attempt, err := b.Store.Attempt(attemptID)
	if err != nil {
		return result, err
	}
	result.Worktree.Released = attempt.ReleasedAt != nil
	result.Worktree.ReleaseState = attempt.ReleaseState
	return result, nil
}

func resultFromProof(task model.Task, attempt model.Attempt, proof model.LandingProof) delivery.Result {
	reason := attempt.LandingReason
	remote := delivery.RemoteDeliveryResult{State: "unverified"}
	if task.CurrentAttemptID == attempt.ID {
		if task.LandedReason != "" {
			reason = task.LandedReason
		}
		remote = delivery.RemoteDeliveryResult{State: task.RemoteDeliveryState, PRState: task.PRState}
	}
	if proof.Kind == "github_pr_attested_ancestry" {
		remote.State = "externally_attested"
		remote.PRState = "MERGED"
	}
	if reason == "" {
		switch proof.Kind {
		case "github_pr_attested_ancestry":
			reason = fmt.Sprintf("driver-attested PR ancestry previously verified for sealed commit %s into %s; original artifact was unchanged", proof.SourceCommit, proof.TargetRef)
		case model.LandingKindLocalAttestedAncestry:
			reason = fmt.Sprintf("driver-attested local recovery previously verified for sealed commit %s into %s; original artifact was unchanged", proof.SourceCommit, proof.TargetRef)
		case "github_pr":
			reason = fmt.Sprintf("GitHub PR landing previously verified for sealed commit %s into %s", proof.SourceCommit, proof.TargetRef)
		case "report_artifact":
			reason = "report artifact was previously verified"
		default:
			reason = fmt.Sprintf("landed locally: worker %s is contained in %s at %s; remote delivery was not verified", proof.SourceCommit, proof.TargetRef, proof.TargetCommit)
		}
	}
	return delivery.Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done", CompletionProvenance: task.CompletionProvenance, Landed: true, Reason: reason,
		Landing: delivery.LandingResult{Landed: true, Kind: proof.Kind, SourceCommit: proof.SourceCommit, TargetRef: proof.TargetRef,
			TargetCommit: proof.TargetCommit, CheckpointRevision: proof.CheckpointRevision, VerifiedAt: proof.VerifiedAt},
		RemoteDelivery: remote,
		Worktree:       delivery.WorktreeResult{Released: attempt.ReleasedAt != nil, ReleaseState: attempt.ReleaseState}}
}

func sameExternalEvidence(attestation, evidence model.ExternalDeliveryAttestation) bool {
	return attestation.Provider == evidence.Provider && attestation.RemoteHost == evidence.RemoteHost && attestation.RemoteRepository == evidence.RemoteRepository && attestation.PRNumber == evidence.PRNumber && attestation.PRNodeID == evidence.PRNodeID && attestation.PRURL == evidence.PRURL && attestation.RegisteredDefaultBranch == evidence.RegisteredDefaultBranch && attestation.PRBaseRef == evidence.PRBaseRef && attestation.PRHeadRef == evidence.PRHeadRef && attestation.PRHeadCommit == evidence.PRHeadCommit && attestation.MergeCommit == evidence.MergeCommit && attestation.MergedAt == evidence.MergedAt && attestation.GraphValidation == evidence.GraphValidation
}
