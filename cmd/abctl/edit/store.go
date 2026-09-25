package edit

import (
	"context"
	"time"
)

// Target is how a Store describes itself to the operator watching the edit.
//
// Every field exists because the other backend's words were wrong for it. A
// local edit that reported "Fetching ConfigMap…" named a thing that does not
// exist; the cluster path's "up to 120s while kubelet syncs" is off by two
// orders of magnitude against a file the proxy is already watching; and
// blaming a dropped port-forward makes no sense where there is no
// port-forward. None of it is inferable from the overlay, and having the store
// say it keeps the renderer from type-switching on the backend.
type Target struct {
	// Noun completes "Fetching %s…", "Applying to %s…" and "Restoring
	// previous %s…", so it reads as a thing, not a sentence.
	Noun string
	// WaitHint explains why the reload takes as long as it does. Rendered
	// parenthesized under "Waiting for hot-reload…".
	WaitHint string
	// UnreachableHint completes "reload status endpoint unreachable (%s)" when
	// the poll gives up after consecutive transport failures. Phrased as the
	// question the operator should chase.
	UnreachableHint string
	// OutOfSyncHint completes "…are out of sync; %s" on the worst path, where
	// both the reload and the rollback failed. The one place the operator has
	// to go look at something themselves, so it names what — a tool for the
	// cluster, the actual file path locally.
	OutOfSyncHint string
	// PollDeadline bounds how long to wait for the reload before declaring it
	// stuck and rolling back. A ceiling, not an expectation: the poll returns
	// the instant last_success moves, so this only sets how long a FAILED
	// reload takes to be called.
	//
	// Per-store because one 120s constant sized for a kubelet ConfigMap sync
	// made a local edit — which lands in about a second — take two minutes to
	// admit it had failed, exactly when something was already wrong.
	PollDeadline time.Duration
}

// Store is where a pipeline lives: the editor fetches the runtime YAML from
// one, splices the user's edit into it, and applies it back.
//
// Two implementations, one per deployment shape abctl can be pointed at:
// ConfigMapStore (a pod's ConfigMap, reached through kubectl) and FileStore
// (the config file of a Cortex running on this machine). Both are driven by
// the same tea.Cmds in edit.go, so the diff prompt, the templates, the
// validation and the /reload/status poll are written once.
//
// The seam is here rather than at Runner because Runner is "a thing that
// invokes kubectl" — a file backend cannot implement it. What the flow
// actually needs is fetch / rewrap / apply, which is what this is.
type Store interface {
	// Fetch reads the runtime YAML and locates the pipeline subtree in it.
	Fetch(ctx context.Context) (*FetchedPipeline, error)

	// Describe names this target for the edit overlay.
	Describe() Target

	// Build wraps a spliced runtime YAML into whatever the target accepts.
	// The caller splices (see Splice); Build only re-wraps, so the ConfigMap
	// implementation re-emits a manifest and the file implementation has
	// nothing to do.
	Build(orig *FetchedPipeline, newInner []byte) ([]byte, error)

	// Apply writes Build's output back and returns the instant the write was
	// issued. PollUntilReloaded compares last_success against that instant
	// with sub-second precision, so it must be captured before the write can
	// possibly have been picked up.
	Apply(ctx context.Context, payload []byte) (time.Time, error)
}

// ConflictChecker is implemented by stores that can tell whether the thing they
// fetched has been written by somebody else since. ApplyCmd consults it before
// a forward apply; a store that does not implement it applies unconditionally.
//
// Optional rather than part of Store because the two backends have genuinely
// different semantics. ConfigMapStore deliberately wins conflicts — it applies
// with --force-conflicts=true and takes field-manager ownership, which is what
// makes editing an operator-owned ConfigMap possible at all. FileStore has no
// such arbiter: a rename lands on whatever is there.
type ConflictChecker interface {
	// CheckUnchanged returns a non-nil error if the stored config no longer
	// matches what orig captured.
	CheckUnchanged(ctx context.Context, orig *FetchedPipeline) error
}

// ConfigMapStore edits the per-agent ConfigMap of a pod in a cluster, through
// kubectl. This is the picker's store: it needs a namespace and a pod, which
// only the Namespaces → Pods flow can supply.
type ConfigMapStore struct {
	Run       Runner
	Namespace string
	Pod       string
}

// Fetch resolves the pod's agent name and reads authbridge-config-<agent>.
//
// The agent-name lookup lives here rather than in the caller because it is
// the ConfigMap naming convention's problem, and a file store has no analogue.
func (s ConfigMapStore) Fetch(ctx context.Context) (*FetchedPipeline, error) {
	agent, err := ResolveAgentName(ctx, s.Run, s.Namespace, s.Pod)
	if err != nil {
		return nil, err
	}
	return Fetch(ctx, s.Run, s.Namespace, agent)
}

func (s ConfigMapStore) Describe() Target {
	return Target{
		Noun:            "ConfigMap",
		WaitHint:        "this can take up to 120s while kubelet syncs the ConfigMap",
		UnreachableHint: "port-forward dropped or framework down?",
		OutOfSyncHint:   "check kubectl",
		PollDeadline:    ClusterPollDeadline,
	}
}

func (s ConfigMapStore) Build(orig *FetchedPipeline, newInner []byte) ([]byte, error) {
	return BuildManifest(orig.Original, newInner)
}

func (s ConfigMapStore) Apply(ctx context.Context, payload []byte) (time.Time, error) {
	return Apply(ctx, s.Run, payload)
}
