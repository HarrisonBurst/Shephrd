package model

import "time"

type Repo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	DefaultBranch string    `json:"default_branch"`
	ContextFile   string    `json:"context_file,omitempty"`
	SetupHook     string    `json:"setup_hook,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Task struct {
	ID                   string     `json:"id"`
	RepoID               string     `json:"repo_id"`
	RepoName             string     `json:"repo_name,omitempty"`
	RepoPath             string     `json:"repo_path,omitempty"`
	FeatureKey           string     `json:"feature_key"`
	Title                string     `json:"title"`
	DriverID             string     `json:"driver_id"`
	Objective            string     `json:"objective"`
	AcceptanceCriteria   string     `json:"acceptance_criteria,omitempty"`
	Deliverable          string     `json:"deliverable"`
	Status               string     `json:"status"`
	CurrentAttemptID     string     `json:"current_attempt_id,omitempty"`
	ArtifactRef          string     `json:"artifact_ref,omitempty"`
	ClaimedDone          bool       `json:"claimed_done"`
	CompletionProvenance string     `json:"completion_provenance,omitempty"`
	ProcessAlive         bool       `json:"process_alive"`
	BranchPushed         bool       `json:"branch_pushed"`
	RemoteDeliveryState  string     `json:"remote_delivery_state"`
	PRState              string     `json:"pr_state,omitempty"`
	Landed               bool       `json:"landed"`
	LandedReason         string     `json:"landed_reason,omitempty"`
	DiscardAuthorized    bool       `json:"discard_authorized"`
	ArchivedAt           *time.Time `json:"archived_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type Attempt struct {
	ID                       string                `json:"id"`
	TaskID                   string                `json:"task_id"`
	Number                   int                   `json:"number"`
	Harness                  string                `json:"harness"`
	Model                    string                `json:"model"`
	RuntimeBackend           string                `json:"runtime_backend"`
	RuntimeGeneration        int                   `json:"runtime_generation"`
	RuntimeExecutable        string                `json:"runtime_executable,omitempty"`
	RunGeneration            int                   `json:"run_generation"`
	ReportPath               string                `json:"report_path,omitempty"`
	ResumeSourceAttemptID    string                `json:"resume_source_attempt_id,omitempty"`
	ResumeSourceRevision     int                   `json:"resume_source_revision,omitempty"`
	TerminalEndpoint         *TerminalEndpoint     `json:"terminal_endpoint,omitempty"`
	TerminalCreateIntent     *TerminalCreateIntent `json:"terminal_create_intent,omitempty"`
	SessionID                string                `json:"session_id,omitempty"`
	WorkspaceBackend         string                `json:"workspace_backend,omitempty"`
	WorkspaceState           string                `json:"workspace_state,omitempty"`
	WorkspaceStateChangedAt  *time.Time            `json:"workspace_state_changed_at,omitempty"`
	IntendedWorktreePath     string                `json:"intended_worktree_path,omitempty"`
	WorktreePath             string                `json:"worktree_path,omitempty"`
	WorktreeGitDir           string                `json:"worktree_git_dir,omitempty"`
	WorktreeCommonDir        string                `json:"worktree_common_dir,omitempty"`
	LeaseID                  string                `json:"lease_id,omitempty"`
	Branch                   string                `json:"branch,omitempty"`
	Status                   string                `json:"status"`
	RunnerPID                int                   `json:"runner_pid,omitempty"`
	Cursor                   int64                 `json:"cursor"`
	ExitCode                 *int                  `json:"exit_code,omitempty"`
	FailureReason            string                `json:"failure_reason,omitempty"`
	BaseCommit               string                `json:"base_commit,omitempty"`
	BaseStrategy             string                `json:"base_strategy"`
	BaseRef                  string                `json:"base_ref,omitempty"`
	LandedProven             bool                  `json:"landed_proven"`
	LandingKind              string                `json:"landing_kind,omitempty"`
	LandedSourceCommit       string                `json:"landed_source_commit,omitempty"`
	LandedTargetRef          string                `json:"landed_target_ref,omitempty"`
	LandedTargetCommit       string                `json:"landed_target_commit,omitempty"`
	LandedCheckpointRevision int                   `json:"landed_checkpoint_revision,omitempty"`
	LandedVerifiedAt         *time.Time            `json:"landed_verified_at,omitempty"`
	LandingReason            string                `json:"landing_reason,omitempty"`
	LandingQuarantineReason  string                `json:"landing_quarantine_reason,omitempty"`
	DiscardAuthorized        bool                  `json:"discard_authorized"`
	ReleaseState             string                `json:"release_state"`
	ReleaseClaimedAt         *time.Time            `json:"release_claimed_at,omitempty"`
	ReleaseOwnerPID          int                   `json:"release_owner_pid,omitempty"`
	ReleaseReason            string                `json:"release_reason,omitempty"`
	ReleasedAt               *time.Time            `json:"released_at,omitempty"`
	CreatedAt                time.Time             `json:"created_at"`
	UpdatedAt                time.Time             `json:"updated_at"`
	EndedAt                  *time.Time            `json:"ended_at,omitempty"`
}

type TerminalEndpoint struct {
	Backend         string   `json:"backend"`
	SocketPath      string   `json:"socket_path"`
	WindowID        string   `json:"window_id,omitempty"`
	WorkspaceID     string   `json:"workspace_id"`
	TabID           string   `json:"tab_id,omitempty"`
	PaneID          string   `json:"pane_id"`
	SurfaceID       string   `json:"surface_id,omitempty"`
	ProviderVersion string   `json:"provider_version,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

const (
	TerminalCreateIntentPending   = "pending"
	TerminalCreateIntentCommitted = "committed"
	TerminalCreateIntentAborted   = "aborted"
	TerminalCreateIntentAbandoned = "abandoned"
)

// A pending TerminalCreateIntent blocks new terminal launches for the
// attempt because a terminal endpoint side effect may exist without a
// durable identity.
type TerminalCreateIntent struct {
	State         string `json:"state"`
	RunGeneration int    `json:"run_generation"`
	Backend       string `json:"backend"`
	Source        string `json:"source"`
	WindowID      string `json:"window_id,omitempty"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	CWD           string `json:"cwd"`
	Label         string `json:"label"`
	Generation    int    `json:"generation"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type LandingProof struct {
	TaskID             string    `json:"task_id"`
	AttemptID          string    `json:"attempt_id"`
	RunGeneration      int       `json:"run_generation"`
	Kind               string    `json:"kind"`
	SourceCommit       string    `json:"source_commit"`
	TargetRef          string    `json:"target_ref"`
	TargetCommit       string    `json:"target_commit"`
	CheckpointRevision int       `json:"checkpoint_revision"`
	VerifiedAt         time.Time `json:"verified_at"`
}

type ExternalDeliveryAttestation struct {
	ID                      string    `json:"id"`
	SchemaVersion           int       `json:"schema_version"`
	TaskID                  string    `json:"task_id"`
	AttemptID               string    `json:"attempt_id"`
	RepoID                  string    `json:"repo_id"`
	RunGeneration           int       `json:"run_generation"`
	DoneMessageID           int64     `json:"done_message_id"`
	CheckpointRevision      int       `json:"checkpoint_revision"`
	OriginalArtifactRef     string    `json:"original_artifact_ref"`
	SealedCommit            string    `json:"sealed_commit"`
	Provider                string    `json:"provider"`
	RemoteHost              string    `json:"remote_host"`
	RemoteRepository        string    `json:"remote_repository"`
	PRNumber                int       `json:"pr_number"`
	PRNodeID                string    `json:"pr_node_id"`
	PRURL                   string    `json:"pr_url"`
	RegisteredDefaultBranch string    `json:"registered_default_branch"`
	PRBaseRef               string    `json:"pr_base_ref"`
	PRHeadRef               string    `json:"pr_head_ref"`
	PRHeadCommit            string    `json:"pr_head_commit"`
	MergeCommit             string    `json:"merge_commit"`
	MergedAt                string    `json:"merged_at"`
	DefaultHeadAtValidation string    `json:"default_head_at_validation"`
	GraphValidation         string    `json:"graph_validation"`
	EvidenceDigest          string    `json:"evidence_digest"`
	AttestedByDriverID      string    `json:"attested_by_driver_id"`
	EvidenceValidatedAt     time.Time `json:"evidence_validated_at"`
	CreatedAt               time.Time `json:"created_at"`
}

type DeliveryAttestationCandidate struct {
	Task            Task
	Repo            Repo
	Attempt         Attempt
	Done            Message
	Checkpoint      AttemptCheckpoint
	ExplicitAttempt bool
	Existing        *ExternalDeliveryAttestation
}

// LocalDeliveryRecovery is immutable driver-approved evidence that an already
// accepted done event carries a mismatched branch artifact ref while the
// sealed checkpoint commit is verifiably contained in the registered local
// default branch. The original accepted artifact ref is preserved verbatim;
// the record binds it to the exact attempt branch and sealed commit instead
// of rewriting anything.
type LocalDeliveryRecovery struct {
	ID                      string    `json:"id"`
	SchemaVersion           int       `json:"schema_version"`
	TaskID                  string    `json:"task_id"`
	AttemptID               string    `json:"attempt_id"`
	RepoID                  string    `json:"repo_id"`
	RunGeneration           int       `json:"run_generation"`
	DoneMessageID           int64     `json:"done_message_id"`
	CheckpointRevision      int       `json:"checkpoint_revision"`
	OriginalArtifactRef     string    `json:"original_artifact_ref"`
	AttemptBranch           string    `json:"attempt_branch"`
	SealedCommit            string    `json:"sealed_commit"`
	RegisteredCommonGitDir  string    `json:"registered_common_git_dir"`
	RegisteredDefaultBranch string    `json:"registered_default_branch"`
	DefaultHeadAtValidation string    `json:"default_head_at_validation"`
	AncestryValidation      string    `json:"ancestry_validation"`
	AttestedByDriverID      string    `json:"attested_by_driver_id"`
	EvidenceValidatedAt     time.Time `json:"evidence_validated_at"`
	CreatedAt               time.Time `json:"created_at"`
}

type ReportRecoveryAttestation struct {
	ID                            string    `json:"id"`
	SchemaVersion                 int       `json:"schema_version"`
	TaskID                        string    `json:"task_id"`
	AttemptID                     string    `json:"attempt_id"`
	RepoID                        string    `json:"repo_id"`
	RunGeneration                 int       `json:"run_generation"`
	CheckpointRevision            int       `json:"checkpoint_revision"`
	CheckpointSourceCursor        int64     `json:"checkpoint_source_cursor"`
	CheckpointSessionID           string    `json:"checkpoint_session_id"`
	CheckpointBranch              string    `json:"checkpoint_branch"`
	CheckpointHeadCommit          string    `json:"checkpoint_head_commit"`
	CheckpointWorktreeDirty       bool      `json:"checkpoint_worktree_dirty"`
	CheckpointWorkspaceFactsError string    `json:"checkpoint_workspace_facts_error"`
	WorkspaceBackend              string    `json:"workspace_backend"`
	WorkspaceState                string    `json:"workspace_state"`
	WorktreePath                  string    `json:"worktree_path"`
	WorktreeGitDir                string    `json:"worktree_git_dir"`
	WorktreeCommonDir             string    `json:"worktree_common_dir"`
	CanonicalReportPath           string    `json:"canonical_report_path"`
	FileIdentity                  string    `json:"file_identity"`
	FileMode                      uint32    `json:"file_mode"`
	FileModTimeUnixNano           int64     `json:"file_mod_time_unix_nano"`
	SHA256                        string    `json:"sha256"`
	SizeBytes                     int64     `json:"size_bytes"`
	Reason                        string    `json:"reason"`
	ValidationKind                string    `json:"validation_kind"`
	AttestedByDriverID            string    `json:"attested_by_driver_id"`
	EvidenceValidatedAt           time.Time `json:"evidence_validated_at"`
	CreatedAt                     time.Time `json:"created_at"`
}

type ReportRecoveryCandidate struct {
	Task            Task
	Repo            Repo
	Attempt         Attempt
	Checkpoint      AttemptCheckpoint
	Existing        *ReportRecoveryAttestation
	RecoveryCommand string
}

type Message struct {
	ID             int64     `json:"id"`
	TaskID         string    `json:"task_id"`
	AttemptID      string    `json:"attempt_id"`
	Direction      string    `json:"direction"`
	Type           string    `json:"type"`
	Payload        string    `json:"payload"`
	ArtifactRef    string    `json:"artifact_ref,omitempty"`
	RunGeneration  int       `json:"run_generation"`
	CheckpointJSON string    `json:"checkpoint_json,omitempty"`
	Stale          bool      `json:"stale"`
	Wake           bool      `json:"wake"`
	SourceCursor   int64     `json:"source_cursor"`
	CreatedAt      time.Time `json:"created_at"`
}

const SettledMessageType = "settled"

type VerifiedArtifact struct {
	ID                   string    `json:"id"`
	ProducerTaskID       string    `json:"producer_task_id"`
	ProducerAttemptID    string    `json:"producer_attempt_id"`
	DoneMessageID        int64     `json:"done_message_id"`
	ReportRecoveryID     string    `json:"report_recovery_id,omitempty"`
	Kind                 string    `json:"kind"`
	OriginalRef          string    `json:"original_ref"`
	SHA256               string    `json:"sha256"`
	SizeBytes            int64     `json:"size_bytes"`
	SnapshotPath         string    `json:"snapshot_path"`
	VerifiedAt           time.Time `json:"verified_at"`
	AcceptedEventID      string    `json:"accepted_event_id,omitempty"`
	AcceptedEventName    string    `json:"accepted_event_name,omitempty"`
	AcceptedEventVersion int       `json:"accepted_event_version,omitempty"`
}

const (
	ReportAcceptedEventName             = "report.accepted"
	ReportAcceptedEventVersion          = 1
	ReportVerificationFailedMessageType = "report-verification-failed"
)

type ReportAcceptedHandlerBinding struct {
	Name              string
	ExtensionID       string
	ConfigurationHash string
}

type ReportLifecycleInvocation struct {
	ID                   string     `json:"id"`
	EventID              string     `json:"event_id"`
	EventName            string     `json:"event_name"`
	EventVersion         int        `json:"event_version"`
	ArtifactID           string     `json:"artifact_id"`
	TaskID               string     `json:"task_id"`
	AttemptID            string     `json:"attempt_id"`
	HandlerName          string     `json:"handler_name"`
	ExtensionID          string     `json:"extension_id"`
	ConfigurationHash    string     `json:"configuration_hash"`
	Position             int        `json:"position"`
	State                string     `json:"state"`
	Attempts             int        `json:"attempts"`
	Annotation           string     `json:"annotation,omitempty"`
	ReceiptSystem        string     `json:"receipt_system,omitempty"`
	ReceiptID            string     `json:"receipt_id,omitempty"`
	FailureKind          string     `json:"failure_kind,omitempty"`
	FailureMessage       string     `json:"failure_message,omitempty"`
	Effect               string     `json:"effect,omitempty"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	DeadlineAt           *time.Time `json:"deadline_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	Recoverable          bool       `json:"recoverable"`
	RetryCommand         string     `json:"retry_command,omitempty"`
	InvocationClaimToken string     `json:"-"`
}

type ReportInput struct {
	Position           int       `json:"position"`
	ArtifactID         string    `json:"artifact_id"`
	ProducerRepoID     string    `json:"producer_repo_id"`
	ProducerRepoName   string    `json:"producer_repo_name"`
	ProducerTaskID     string    `json:"producer_task_id"`
	ProducerAttemptID  string    `json:"producer_attempt_id"`
	DoneMessageID      int64     `json:"done_message_id"`
	ReportRecoveryID   string    `json:"report_recovery_id,omitempty"`
	Kind               string    `json:"kind"`
	OriginalRef        string    `json:"original_ref"`
	SHA256             string    `json:"sha256"`
	SizeBytes          int64     `json:"size_bytes"`
	SnapshotPath       string    `json:"snapshot_path"`
	VerifiedAt         time.Time `json:"verified_at"`
	AttachedByDriverID string    `json:"attached_by_driver_id"`
	AttachedAt         time.Time `json:"attached_at"`
}

type ReportArtifactSelection struct {
	ProducerTaskID string `json:"producer_task_id"`
	ArtifactID     string `json:"artifact_id"`
}

type AnnotationScope struct {
	TaskID     string
	PlanID     string
	PlanItemID string
}

type AnnotationInput struct {
	Judgment   string
	Reason     string
	NextAction string
}

type Annotation struct {
	ID         string    `json:"id"`
	DriverID   string    `json:"driver_id"`
	TaskID     string    `json:"task_id,omitempty"`
	PlanID     string    `json:"plan_id,omitempty"`
	PlanItemID string    `json:"plan_item_id,omitempty"`
	Revision   int       `json:"revision"`
	Judgment   string    `json:"judgment"`
	Reason     string    `json:"reason,omitempty"`
	NextAction string    `json:"next_action,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Plan struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	DriverID  string     `json:"driver_id"`
	Items     []PlanItem `json:"items,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type PlanSummary struct {
	ID                    string            `json:"id"`
	Name                  string            `json:"name"`
	DriverID              string            `json:"driver_id"`
	Ownership             string            `json:"ownership"`
	ItemCount             int               `json:"item_count"`
	UndispatchedItemCount int               `json:"undispatched_item_count"`
	DispatchedTaskCount   int               `json:"dispatched_task_count"`
	LiveTaskCount         int               `json:"live_task_count"`
	LandedTaskCount       int               `json:"landed_task_count"`
	DispatchedTasks       []PlanTaskSummary `json:"dispatched_tasks"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

type PlanTaskSummary struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	Landed           bool   `json:"landed"`
	CurrentAttemptID string `json:"current_attempt_id,omitempty"`
}

type PlanItem struct {
	ID                 string             `json:"id"`
	PlanID             string             `json:"plan_id"`
	Position           int                `json:"position"`
	FeatureKey         string             `json:"feature_key,omitempty"`
	Title              string             `json:"title"`
	Objective          string             `json:"objective"`
	Description        string             `json:"description,omitempty"`
	AcceptanceCriteria string             `json:"acceptance_criteria,omitempty"`
	RepoID             string             `json:"repo_id,omitempty"`
	RepoName           string             `json:"repo_name,omitempty"`
	RepoPath           string             `json:"repo_path,omitempty"`
	Deliverable        string             `json:"deliverable"`
	DispatchedTaskID   string             `json:"dispatched_task_id,omitempty"`
	DispatchedTask     *Task              `json:"dispatched_task,omitempty"`
	Prerequisites      []PlanPrerequisite `json:"prerequisites"`
	Reports            []PlanReport       `json:"reports"`
	Readiness          PlanReadiness      `json:"readiness"`
	LatestAnnotation   *Annotation        `json:"latest_annotation,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

type PlanPrerequisite struct {
	ItemID           string        `json:"item_id"`
	Title            string        `json:"title"`
	Objective        string        `json:"objective"`
	Position         int           `json:"position"`
	DispatchedTaskID string        `json:"dispatched_task_id,omitempty"`
	Task             *Task         `json:"task,omitempty"`
	Evidence         *TaskEvidence `json:"evidence,omitempty"`
}

type PlanReport struct {
	PrerequisiteItemID string            `json:"prerequisite_item_id"`
	Position           int               `json:"position"`
	ArtifactID         string            `json:"artifact_id,omitempty"`
	Artifact           *VerifiedArtifact `json:"artifact,omitempty"`
	SelectedByDriverID string            `json:"selected_by_driver_id,omitempty"`
	SelectedAt         *time.Time        `json:"selected_at,omitempty"`
	Stale              bool              `json:"stale"`
	StaleReason        string            `json:"stale_reason,omitempty"`
}

type TaskEvidence struct {
	TaskID           string            `json:"task_id"`
	Title            string            `json:"title"`
	FeatureKey       string            `json:"feature_key"`
	Status           string            `json:"status"`
	Landed           bool              `json:"landed"`
	ArtifactRef      string            `json:"artifact_ref,omitempty"`
	CurrentAttemptID string            `json:"current_attempt_id,omitempty"`
	VerifiedReport   *VerifiedArtifact `json:"verified_report,omitempty"`
}

type PlanReadiness struct {
	Ready             bool           `json:"ready"`
	Reasons           []PlanReason   `json:"reasons"`
	RequiredActions   []string       `json:"required_actions"`
	AvailableEvidence []TaskEvidence `json:"available_evidence"`
}

type PlanReason struct {
	Code               string `json:"code"`
	Message            string `json:"message"`
	PrerequisiteItemID string `json:"prerequisite_item_id,omitempty"`
}

// PlanItemRelations are the explicit relations authored with one plan item in
// the same transaction: prerequisite edges and report-input relations.
type PlanItemRelations struct {
	Position int
	Requires []string
	Reports  []string
}

type PlanItemUpdate struct {
	Title              *string
	FeatureKey         *string
	Objective          *string
	Description        *string
	AcceptanceCriteria *string
	RepoID             *string
	Deliverable        *string
	Position           *int
}

type PlanReportSelection struct {
	PrerequisiteItemID string
	ProducerTaskID     string
	ArtifactID         string
}

type SpawnResult struct {
	Task      Task     `json:"task"`
	Attempt   Attempt  `json:"attempt"`
	BriefPath string   `json:"brief_path"`
	Label     string   `json:"label"`
	Warnings  []string `json:"warnings,omitempty"`
}

type PlanDispatch struct {
	Plan      Plan          `json:"plan"`
	Item      PlanItem      `json:"item"`
	Task      Task          `json:"task"`
	Readiness PlanReadiness `json:"readiness"`
	Spawn     *SpawnResult  `json:"spawn,omitempty"`
}

type TaskDetail struct {
	Task                         Task                          `json:"task"`
	Inputs                       []ReportInput                 `json:"inputs"`
	Attempts                     []Attempt                     `json:"attempts"`
	Messages                     []Message                     `json:"messages"`
	Checkpoints                  []AttemptCheckpoint           `json:"checkpoints"`
	Annotations                  []Annotation                  `json:"annotations"`
	Notifications                []DriverNotification          `json:"notifications"`
	ExternalDeliveryAttestations []ExternalDeliveryAttestation `json:"external_delivery_attestations"`
	LocalDeliveryRecoveries      []LocalDeliveryRecovery       `json:"local_delivery_recoveries"`
	ReportRecoveryAttestations   []ReportRecoveryAttestation   `json:"report_recovery_attestations"`
	ReportLifecycleInvocations   []ReportLifecycleInvocation   `json:"report_lifecycle_invocations"`
}

type NotificationState string

const NotificationAckSchemaVersion = 1

const (
	NotificationPending      NotificationState = "pending"
	NotificationClaimed      NotificationState = "claimed"
	NotificationAcknowledged NotificationState = "acknowledged"
	NotificationSuperseded   NotificationState = "superseded"
)

type NotificationClaim struct {
	ConsumerID       string    `json:"consumer_id"`
	DriverGeneration string    `json:"driver_generation"`
	ClaimToken       string    `json:"claim_token"`
	ClaimUntil       time.Time `json:"claim_until"`
}

type DriverNotification struct {
	SubdriverEventID    int64                       `json:"subdriver_event_id,omitempty"`
	RequestID           string                      `json:"request_id,omitempty"`
	SubdriverID         string                      `json:"subdriver_id,omitempty"`
	SubdriverRepoName   string                      `json:"subdriver_repo_name,omitempty"`
	NotificationID      string                      `json:"notification_id"`
	MessageID           int64                       `json:"message_id"`
	TaskID              string                      `json:"task_id"`
	TaskTitle           string                      `json:"task_title"`
	FeatureKey          string                      `json:"feature_key"`
	TaskLabel           string                      `json:"task_label,omitempty"`
	AttemptID           string                      `json:"attempt_id"`
	TargetDriverID      string                      `json:"target_driver_id"`
	WorkerRunGeneration int                         `json:"worker_run_generation"`
	SourceCursor        int64                       `json:"source_cursor"`
	Kind                string                      `json:"kind"`
	State               NotificationState           `json:"state"`
	Payload             string                      `json:"payload"`
	Artifact            string                      `json:"artifact,omitempty"`
	CreatedAt           time.Time                   `json:"created_at"`
	UpdatedAt           time.Time                   `json:"updated_at"`
	ClaimOwner          string                      `json:"claim_owner,omitempty"`
	DriverGeneration    string                      `json:"driver_generation,omitempty"`
	ClaimToken          string                      `json:"claim_token,omitempty"`
	ClaimedAt           *time.Time                  `json:"claimed_at,omitempty"`
	ClaimUntil          *time.Time                  `json:"claim_until,omitempty"`
	DeliveryAttempts    int                         `json:"delivery_attempts"`
	AckedAt             *time.Time                  `json:"acked_at,omitempty"`
	AckOwner            string                      `json:"ack_owner,omitempty"`
	AckDriverGeneration string                      `json:"ack_driver_generation,omitempty"`
	HandlingID          string                      `json:"handling_id,omitempty"`
	SupersededAt        *time.Time                  `json:"superseded_at,omitempty"`
	SupersedeReason     string                      `json:"supersede_reason,omitempty"`
	ReportLifecycle     []ReportLifecycleInvocation `json:"report_lifecycle,omitempty"`
	Claim               *NotificationClaim          `json:"claim,omitempty"`
}

type NotificationDrain struct {
	Notifications    []DriverNotification `json:"notifications"`
	Reclaimed        int                  `json:"reclaimed"`
	Superseded       int                  `json:"superseded"`
	ConsumerID       string               `json:"consumer_id"`
	DriverGeneration string               `json:"driver_generation"`
}

type NotificationAckRequest struct {
	NotificationID   string
	ClaimToken       string
	ConsumerID       string
	DriverGeneration string
	HandlingID       string
}

type NotificationAckReceipt struct {
	SchemaVersion    int       `json:"schema_version"`
	NotificationID   string    `json:"notification_id"`
	MessageID        int64     `json:"message_id"`
	HandlingID       string    `json:"handling_id"`
	AckedAt          time.Time `json:"acked_at"`
	ConsumerID       string    `json:"consumer_id"`
	DriverGeneration string    `json:"driver_generation"`
	Idempotent       bool      `json:"idempotent,omitempty"`
}

type NotificationRenewRequest struct {
	NotificationID   string
	ClaimToken       string
	ConsumerID       string
	DriverGeneration string
}

type NotificationDeliveryLog struct {
	ID               int64     `json:"id"`
	NotificationID   string    `json:"notification_id"`
	TargetDriverID   string    `json:"target_driver_id"`
	Operation        string    `json:"operation"`
	ConsumerID       string    `json:"consumer_id,omitempty"`
	DriverGeneration string    `json:"driver_generation,omitempty"`
	ClaimToken       string    `json:"claim_token,omitempty"`
	HandlingID       string    `json:"handling_id,omitempty"`
	Result           string    `json:"result,omitempty"`
	Detail           string    `json:"detail,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type NotificationCounts struct {
	Pending      int `json:"pending"`
	Claimed      int `json:"claimed"`
	Acknowledged int `json:"acknowledged"`
	Superseded   int `json:"superseded"`
	Expired      int `json:"expired"`
}

type TaskAdoption struct {
	TaskID           string `json:"task_id"`
	PreviousDriverID string `json:"previous_driver_id"`
	DriverID         string `json:"driver_id"`
	Retargeted       int    `json:"retargeted_notifications"`
	ReleasedClaims   int    `json:"released_claims"`
}

type Event struct {
	Type       string      `json:"type"`
	Payload    string      `json:"payload"`
	Artifact   string      `json:"artifact,omitempty"`
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
}

type Checkpoint struct {
	SchemaVersion int        `json:"schema_version"`
	Summary       string     `json:"summary"`
	Completed     []string   `json:"completed"`
	NextSteps     []string   `json:"next_steps"`
	Decisions     []Decision `json:"decisions"`
	ChangedPaths  []string   `json:"changed_paths"`
	Checks        []Check    `json:"checks"`
	Blockers      []string   `json:"blockers"`
}

type Decision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type Check struct {
	Command string `json:"command"`
	Result  string `json:"result"`
}

type WorkspaceFacts struct {
	HeadCommit string `json:"head_commit"`
	Dirty      bool   `json:"dirty"`
	Error      string `json:"error,omitempty"`
}

type AttemptWorkspace struct {
	Backend   string
	SessionID string
	Path      string
	LeaseID   string
	GitDir    string
	CommonDir string
	Branch    string
}

type AttemptCheckpoint struct {
	AttemptID           string     `json:"attempt_id"`
	Revision            int        `json:"revision"`
	SchemaVersion       int        `json:"schema_version"`
	Producer            string     `json:"producer"`
	RunGeneration       int        `json:"run_generation"`
	SourceCursor        int64      `json:"source_cursor"`
	SessionID           string     `json:"session_id,omitempty"`
	Branch              string     `json:"branch,omitempty"`
	HeadCommit          string     `json:"head_commit,omitempty"`
	WorktreeDirty       bool       `json:"worktree_dirty"`
	WorkspaceFactsError string     `json:"workspace_facts_error,omitempty"`
	Summary             string     `json:"summary"`
	Completed           []string   `json:"completed"`
	NextSteps           []string   `json:"next_steps"`
	Decisions           []Decision `json:"decisions"`
	ChangedPaths        []string   `json:"changed_paths"`
	Checks              []Check    `json:"checks"`
	Blockers            []string   `json:"blockers"`
	SourceAttemptID     string     `json:"source_attempt_id,omitempty"`
	SourceRevision      int        `json:"source_revision,omitempty"`
	CapturedAt          time.Time  `json:"captured_at"`
}
