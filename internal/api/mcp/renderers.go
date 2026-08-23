package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The renderer tools (§68, §71).
//
// # Why these exist before any user interface does
//
// Heyarr has no client. Until it has one, the question "play the new episode
// on the living-room television" has no answer that does not involve a
// terminal — and a terminal is not where anyone is standing when they want to
// watch something.
//
// These four tools make a model the remote control. That is a better first
// client than a web page for a reason beyond convenience: an assistant already
// knows how to resolve "the new episode" to a work, which is the hard half of
// a media interface and the half a UI would have to build from scratch. It
// also means the mobile answer, when it comes, is a client of an API that has
// already been used in anger rather than one designed on paper.
//
// # Naming is the whole interface here
//
// A model picks a tool from its description and nothing else, so these say
// what a PERSON would mean. "Play something on a screen or speaker", not
// "issue SetAVTransportURI". The renderer id is a UDN, which no one will ever
// say aloud, so play_here accepts a name and resolves it — matching how
// somebody actually refers to the thing in their living room.

func (s *Server) registerRendererTools() {
	s.tools.register(Tool{
		Name:     "list_renderers",
		Title:    "What can I play on",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "List the televisions, speakers and projectors on the network that " +
			"Heyarr can play to. Use this to resolve what someone means by \"the living " +
			"room\" before playing anything. A device that is switched off will not be " +
			"listed — that is not the same as it not existing, so do not tell someone " +
			"they have no television because a screen was asleep.",
		InputSchema: schemaListRenderers,
		Handler:     s.listRenderers,
	})

	s.tools.register(Tool{
		Name:     "play_here",
		Title:    "Play something on a screen or speaker",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Play an asset on a renderer. Name the renderer the way a person " +
			"would — \"living room\" — rather than by id; the name is matched against " +
			"what the device calls itself. This plans the playback, checks the file " +
			"against what that device says it can decode, and starts it. If the device " +
			"cannot play the file, the refusal explains which codec or container it " +
			"refused, which is the answer someone actually wants.",
		InputSchema: schemaPlayHere,
		Handler:     s.playHere,
	})

	s.tools.register(Tool{
		Name:     "control_playback",
		Title:    "Pause, resume or stop",
		Scope:    auth.ScopeWrite,
		ReadOnly: false,
		Description: "Pause, resume or stop what a renderer is playing. Resume continues " +
			"from where it was paused. Stop ends it and releases the content — use pause " +
			"when someone is coming back.",
		InputSchema: schemaControlPlayback,
		Handler:     s.controlPlayback,
	})

	s.tools.register(Tool{
		Name:     "playback_status",
		Title:    "What is playing",
		Scope:    auth.ScopeRead,
		ReadOnly: true,
		Description: "Report what a renderer is doing and how far into it. A device that " +
			"reports no position is not broken: some report none until they have parsed " +
			"enough of the stream, and some never report one at all.",
		InputSchema: schemaPlaybackStatus,
		Handler:     s.playbackStatus,
	})
}

var schemaListRenderers = obj(map[string]any{
	"refresh": map[string]any{
		"type": "boolean",
		"description": "Search the network again rather than reusing the last result. " +
			"Use this when someone has just switched a device on, and not otherwise — " +
			"a search takes several seconds.",
	},
})

var schemaPlayHere = obj(map[string]any{
	"asset_id": map[string]any{
		"type":        "string",
		"description": "The asset to play. Resolve it with search_content first.",
	},
	"renderer": map[string]any{
		"type": "string",
		"description": "Which device, by name or id. A partial name is matched against " +
			"what each device calls itself, so \"living\" finds \"Samsung QN85BA 55\" " +
			"only if that is what it is called — prefer list_renderers when unsure.",
	},
})

var schemaControlPlayback = obj(map[string]any{
	"renderer": map[string]any{"type": "string", "description": "Which device, by name or id."},
	"action": map[string]any{
		"type": "string", "enum": []any{"pause", "resume", "stop"},
		"description": "What to do.",
	},
})

var schemaPlaybackStatus = obj(map[string]any{
	"renderer": map[string]any{"type": "string", "description": "Which device, by name or id."},
})

func (s *Server) listRenderers(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Refresh bool `json:"refresh"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	found, err := s.resources.RenderersFor(ctx, args.Refresh)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		// Said explicitly, because "[]" invites the wrong conclusion. A
		// screen in standby is invisible to discovery and this must not be
		// reported as "you have no televisions".
		return map[string]any{
			"renderers": found,
			"note": "nothing answered. A device that is switched off does not appear — " +
				"ask whether the screen is on before concluding there is none.",
		}, nil
	}
	return map[string]any{"renderers": found}, nil
}

func (s *Server) playHere(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		AssetID  string `json:"asset_id"`
		Renderer string `json:"renderer"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.AssetID) == "" {
		return nil, invalidParams("give me an asset_id — resolve it with search_content first")
	}
	udn, err := s.resolveRenderer(ctx, args.Renderer)
	if err != nil {
		return nil, err
	}
	return s.resources.PlayOnRenderer(ctx, udn, args.AssetID)
}

func (s *Server) controlPlayback(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Renderer string `json:"renderer"`
		Action   string `json:"action"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	switch args.Action {
	case "pause", "resume", "stop":
	default:
		return nil, invalidParams("action must be pause, resume or stop")
	}
	udn, err := s.resolveRenderer(ctx, args.Renderer)
	if err != nil {
		return nil, err
	}
	return s.resources.ControlRenderer(ctx, udn, args.Action)
}

func (s *Server) playbackStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Renderer string `json:"renderer"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	udn, err := s.resolveRenderer(ctx, args.Renderer)
	if err != nil {
		return nil, err
	}
	return s.resources.RendererStatusFor(ctx, udn)
}

// resolveRenderer turns what someone said into a UDN.
//
// Nobody says "uuid:9cf4b79e-8ddf-4f8d-a3e3-9266fb4f5484". They say "the
// living room", so a name is matched case-insensitively as a substring of what
// the device calls itself.
//
// An ambiguous name is an ERROR listing the matches rather than a guess. There
// are two televisions in this house and picking the wrong one puts something
// on a screen in a room nobody is in — a failure the person cannot see and
// will not understand.
func (s *Server) resolveRenderer(ctx context.Context, want string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", invalidParams("say which device — use list_renderers to see them")
	}
	found, err := s.resources.RenderersFor(ctx, false)
	if err != nil {
		return "", err
	}

	var matches []string
	var names []string
	for _, r := range found {
		names = append(names, r.Name)
		if r.UDN == want {
			return r.UDN, nil
		}
		if strings.Contains(strings.ToLower(r.Name), strings.ToLower(want)) {
			matches = append(matches, r.UDN)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", invalidParams("no device matches %s. Known devices: %s. "+
			"A device that is switched off will not be here.", want, strings.Join(names, ", "))
	default:
		return "", invalidParams("%s matches more than one device: %s. Say which.",
			want, strings.Join(names, ", "))
	}
}
