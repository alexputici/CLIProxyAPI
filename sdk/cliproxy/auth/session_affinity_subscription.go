package auth

import (
	"context"
	"strings"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// recoveredClaudeSubscriptions uses the already eligible candidates. Priority,
// model support, cooldown, caller restrictions and tried credentials remain the
// responsibility of the existing selection pipeline.
func (s *SessionAffinitySelector) recoveredClaudeSubscriptions(opts cliproxyexecutor.Options, bound *Auth, available []*Auth) []*Auth {
	if !s.preferClaudeSubscription || s.rebindOnNever || bound == nil ||
		!strings.EqualFold(strings.TrimSpace(bound.Provider), "claude") || bound.AuthKind() != AuthKindAPIKey ||
		pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil
	}
	var subscriptions []*Auth
	for _, candidate := range highestPriorityAuths(available) {
		if candidate != nil && strings.EqualFold(strings.TrimSpace(candidate.Provider), "claude") && candidate.AuthKind() == AuthKindOAuth {
			subscriptions = append(subscriptions, candidate)
		}
	}
	if len(subscriptions) == 0 || claudeRequestHasOwnedResources(opts.OriginalRequest) {
		return nil
	}
	return subscriptions
}

func (s *SessionAffinitySelector) pickBoundWithClaudeSubscription(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, available []*Auth, bound *Auth, reservation **weightedReservation) (*Auth, error) {
	if subscriptions := s.recoveredClaudeSubscriptions(opts, bound, available); len(subscriptions) > 0 {
		picked, errPick := s.pickWithAffinityReservation(ctx, provider, model, opts, subscriptions, nil, reservation)
		if picked == nil && errPick == nil {
			return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return picked, errPick
	}
	return s.pickWithAffinityReservation(ctx, provider, model, opts, available, bound, reservation)
}

// Anthropic file IDs are workspace-scoped. Encrypted advisor results, containers and Thread continuations
// also refer to hosted state; recovery cannot assume another account owns it.
// Only inspect native protocol fields, never arbitrary tool-result JSON or text.
// Manager-owned Thread pins remain authoritative independently of this guard.
func claudeRequestHasOwnedResources(body []byte) bool {
	if ojson.GetBytesNoCopy(body, "thread.previous_message_id").String() != "" {
		return true
	}
	container := ojson.GetBytesNoCopy(body, "container")
	if container.String() != "" && !container.IsObject() || container.Get("id").String() != "" {
		return true
	}
	var hasResource bool
	container.Get("skills").ForEach(func(_, skill ojson.Result) bool {
		hasResource = skill.Get("type").String() != "anthropic" && skill.Get("skill_id").String() != ""
		return !hasResource
	})
	if hasResource {
		return true
	}
	var inspectBlocks func(ojson.Result)
	inspectBlocks = func(blocks ojson.Result) {
		if !blocks.IsArray() {
			return
		}
		blocks.ForEach(func(_, block ojson.Result) bool {
			switch block.Get("type").String() {
			case "document", "image":
				hasResource = block.Get("source.type").String() == "file" && block.Get("source.file_id").String() != ""
			case "server_tool_use":
				hasResource = block.Get("name").String() == "advisor"
			case "advisor_tool_result", "advisor_redacted_result":
				hasResource = true
			case "container_upload":
				hasResource = block.Get("file_id").String() != ""
			case "tool_result":
				inner := block.Get("content")
				if inner.IsObject() && inner.Get("type").String() == "advisor_redacted_result" {
					hasResource = true
				} else {
					inspectBlocks(inner)
				}
			}
			return !hasResource
		})
	}
	ojson.GetBytesNoCopy(body, "messages").ForEach(func(_, message ojson.Result) bool {
		inspectBlocks(message.Get("content"))
		return !hasResource
	})
	return hasResource
}
