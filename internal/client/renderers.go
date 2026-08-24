package client

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Renderer control from a client (§68).
//
// Every one of these is a thin call onto /api/v1/renderers. The discovery, the
// SOAP and the capability minting all happen on the server, and deliberately:
// SSDP is multicast and does not cross a routed link, and a Samsung's DLNA
// renderer refuses control from off its own subnet. A CLI that spoke UPnP
// itself would work only from a laptop already sitting in the living room.

// Renderer is a device that can be played to.
type Renderer struct {
	UDN          string `json:"udn"`
	Name         string `json:"name"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Location     string `json:"location"`
	State        string `json:"state,omitempty"`
}

// RendererList is the GET /renderers response.
type RendererList struct {
	Renderers []Renderer `json:"renderers"`
}

// RendererStatus is what a renderer is doing.
type RendererStatus struct {
	Renderer string  `json:"renderer"`
	State    string  `json:"state"`
	Playing  bool    `json:"playing"`
	Elapsed  float64 `json:"elapsed_seconds,omitempty"`
	Duration float64 `json:"duration_seconds,omitempty"`
}

// PlayResult is what came of asking a renderer to play something.
type PlayResult struct {
	Playing   string `json:"playing"`
	On        string `json:"on"`
	SessionID string `json:"session_id"`
	Decision  string `json:"decision"`
}

// Renderers lists what can be played to.
//
// refresh searches the network again instead of reusing the server's cached
// sweep. It costs seconds, so it is for "I have just switched it on" and not
// for every invocation.
func (c *Client) Renderers(ctx context.Context, refresh bool) ([]Renderer, error) {
	var q url.Values
	if refresh {
		q = url.Values{"refresh": []string{"true"}}
	}
	var out RendererList
	if err := c.Get(ctx, "/renderers", q, &out); err != nil {
		return nil, err
	}
	return out.Renderers, nil
}

// ResolveRenderer turns what someone typed into a UDN.
//
// Nobody types a UDN. They type "living room", so a name is matched
// case-insensitively as a substring of what the device calls itself.
//
// An ambiguous name is an error listing the candidates rather than a guess.
// A household with two televisions is ordinary, and picking the wrong one puts
// something on a screen in a room nobody is in — a failure the person standing
// in the other room cannot see and will not understand.
func (c *Client) ResolveRenderer(ctx context.Context, want string) (Renderer, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Renderer{}, fmt.Errorf("say which device — `heyarr renderers discover` lists them")
	}
	found, err := c.Renderers(ctx, false)
	if err != nil {
		return Renderer{}, err
	}

	var matches []Renderer
	names := make([]string, 0, len(found))
	for _, r := range found {
		names = append(names, r.Name)
		if r.UDN == want {
			return r, nil
		}
		if strings.Contains(strings.ToLower(r.Name), strings.ToLower(want)) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(names) == 0 {
			// The empty case is spelled out, because "no device matches" over
			// an empty list reads as "that name is wrong" when the real
			// answer is that nothing answered at all.
			return Renderer{}, fmt.Errorf(
				"no renderers are known — nothing answered the last search. " +
					"A device that is switched off is invisible to discovery; " +
					"switch it on and run `heyarr renderers discover`")
		}
		return Renderer{}, fmt.Errorf("no device matches %q. Known: %s",
			want, strings.Join(names, ", "))
	default:
		matched := make([]string, 0, len(matches))
		for _, m := range matches {
			matched = append(matched, m.Name)
		}
		return Renderer{}, fmt.Errorf("%q matches more than one device: %s — say which",
			want, strings.Join(matched, ", "))
	}
}

// PlayOnRenderer plans a playback and pushes it to a renderer.
func (c *Client) PlayOnRenderer(ctx context.Context, udn, assetID string) (PlayResult, error) {
	var out PlayResult
	err := c.Post(ctx, "/renderers/"+url.PathEscape(udn)+"/play",
		map[string]string{"asset_id": assetID}, &out)
	return out, err
}

// ControlRenderer applies pause, resume or stop and returns the state after it.
func (c *Client) ControlRenderer(ctx context.Context, udn, action string) (RendererStatus, error) {
	var out RendererStatus
	err := c.Post(ctx, "/renderers/"+url.PathEscape(udn)+"/"+action, nil, &out)
	return out, err
}

// SeekRenderer jumps to an absolute offset from the start.
func (c *Client) SeekRenderer(ctx context.Context, udn string, seconds float64) (RendererStatus, error) {
	var out RendererStatus
	err := c.Post(ctx, "/renderers/"+url.PathEscape(udn)+"/seek",
		map[string]float64{"seconds": seconds}, &out)
	return out, err
}

// RendererStatusFor reports what a renderer is doing.
func (c *Client) RendererStatusFor(ctx context.Context, udn string) (RendererStatus, error) {
	var out RendererStatus
	err := c.Get(ctx, "/renderers/"+url.PathEscape(udn)+"/status", nil, &out)
	return out, err
}
