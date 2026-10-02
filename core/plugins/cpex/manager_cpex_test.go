// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"strings"
	"testing"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestUnchangedPayloadIsNotApplied(t *testing.T) {
	// CPEX 0.2.3 returns a final payload on every allowed pipeline. Its
	// presence alone must not trigger a body rewrite or a modify decision.
	pres := &rcpex.PipelineResult{
		ContinueProcessing: true,
		ModifiedPayload:    []byte("not a MessagePack payload"),
	}
	if err := applyModificationsToPctx(&pipeline.Context{}, pres, false); err != nil {
		t.Fatalf("unchanged payload was decoded: %v", err)
	}
	if got := mapResult(pres).Decision; got != DecisionAllow {
		t.Fatalf("decision = %v, want allow", got)
	}
}

func TestModifiedPayloadRequiresBytes(t *testing.T) {
	pres := &rcpex.PipelineResult{
		ContinueProcessing: true,
		PayloadModified:    true,
	}
	if err := applyModificationsToPctx(&pipeline.Context{}, pres, false); err == nil || !strings.Contains(err.Error(), "no payload") {
		t.Fatalf("missing modified payload error = %v", err)
	}
	if got := mapResult(pres).Decision; got != DecisionModify {
		t.Fatalf("decision = %v, want modify", got)
	}
}
