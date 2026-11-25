package auth

import (
	"context"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// betterFundedAuths returns the credentials a bound session should move to
// because they are in a strictly better funding class than the one it holds,
// or nil when the binding should stay.
//
// Only credentials of the bound provider are considered, since a conversation
// cannot continue on another provider. The result is the best funding class
// present, narrowed to its highest priority tier. Model support, cooldowns,
// caller restrictions and tried credentials were applied to available by the
// selection pipeline. Explicit pins and requests that continue state held by
// the bound account keep their binding.
func (s *SessionAffinitySelector) betterFundedAuths(opts cliproxyexecutor.Options, bound *Auth, available []*Auth, now time.Time) []*Auth {
	if len(s.funding) == 0 || s.rebindOnNever || bound == nil || pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil
	}
	var sameProvider []*Auth
	for _, candidate := range available {
		if candidate != nil && strings.EqualFold(strings.TrimSpace(candidate.Provider), strings.TrimSpace(bound.Provider)) {
			sameProvider = append(sameProvider, candidate)
		}
	}
	better := s.funding.narrow(sameProvider, now)
	if len(better) == 0 || s.funding.rank(fundingClassOf(better[0], now)) >= s.funding.rank(fundingClassOf(bound, now)) {
		return nil
	}
	if requestContinuesAccountState(opts.OriginalRequest) {
		return nil
	}
	return highestPriorityAuths(better)
}

func (s *SessionAffinitySelector) pickBoundWithFunding(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, available []*Auth, bound *Auth, now time.Time, reservation **weightedReservation) (*Auth, error) {
	if better := s.betterFundedAuths(opts, bound, available, now); len(better) > 0 {
		picked, errPick := s.pickWithAffinityReservation(ctx, provider, model, opts, better, nil, reservation)
		if picked == nil && errPick == nil {
			return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return picked, errPick
	}
	return s.pickWithAffinityReservation(ctx, provider, model, opts, available, bound, reservation)
}

// requestContinuesAccountState reports whether a request refers to state that
// only the account serving the conversation holds, so it cannot be moved.
func requestContinuesAccountState(body []byte) bool {
	return claudeRequestHasOwnedResources(body) || responsesRequestContinuesStoredState(body)
}

// A Responses request relies on state stored for the serving account when it
// names a previous response or a conversation, or refers to an uploaded file,
// a container or a vector store. A request that carries its full input and
// none of these is portable. A WebSocket create frame nests the same fields
// under "response", and a steer frame names its parent response.
func responsesRequestContinuesStoredState(body []byte) bool {
	if ojson.GetBytesNoCopy(body, "steer.previous_response_id").String() != "" {
		return true
	}
	for _, prefix := range []string{"", "response."} {
		if ojson.GetBytesNoCopy(body, prefix+"previous_response_id").String() != "" {
			return true
		}
		conversation := ojson.GetBytesNoCopy(body, prefix+"conversation")
		if conversation.String() != "" && !conversation.IsObject() || conversation.Get("id").String() != "" {
			return true
		}
		var hasReference bool
		ojson.GetBytesNoCopy(body, prefix+"input").ForEach(func(_, item ojson.Result) bool {
			item.Get("content").ForEach(func(_, part ojson.Result) bool {
				hasReference = part.Get("file_id").String() != ""
				return !hasReference
			})
			return !hasReference
		})
		if hasReference {
			return true
		}
		ojson.GetBytesNoCopy(body, prefix+"tools").ForEach(func(_, tool ojson.Result) bool {
			container := tool.Get("container")
			hasReference = container.String() != "" && !container.IsObject() || container.Get("file_ids.0").String() != "" ||
				tool.Get("vector_store_ids.0").String() != ""
			return !hasReference
		})
		if hasReference {
			return true
		}
	}
	return false
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
