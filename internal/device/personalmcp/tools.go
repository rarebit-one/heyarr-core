package personalmcp

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/device"
)

// instructions are what an agent reads before it chooses a tool. The caveat is
// first because it is the thing an agent is most likely to get wrong: a key
// that exists looks like a key that works.
const instructions = `This is the Personal MCP for one machine's Heyarr device key (spec §40, §73).

It runs on the user's own computer, over stdio, and talks to no Heyarr
controller. The private key never leaves this machine and is never returned by
any tool here — you can see the public key and the path, and nothing else.

The key does not authorise anything yet. It is not enrolled with a user
identity, and every grant against a controller is still an ADR-0011 bearer
token scope until Milestone 8. Do not present it to a user as a working
credential, and do not offer to sign, enrol, pair or recover with it: those
verbs do not exist here because the capability does not exist yet.`

// obj is a small helper so the schemas read as shapes rather than as maps.
func obj(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// registerTools declares the whole surface.
//
// Four verbs, because this device can do four things. Everything §41 and
// ADR-0022 describe — wrapping a space key, delegating, granting, pairing,
// recovering — is absent rather than stubbed: a tool that answers "not
// implemented" is a published promise with a hole in it (ADR-0019).
func (s *Server) registerTools() {
	s.register(Tool{
		Name:  "device_generate",
		Title: "Generate this machine's device key",
		Description: "Create the Ed25519 device key for this machine, written 0600 in the user's " +
			"config directory. Reach for this once, on a machine that has no key yet. " +
			"It refuses if a key already exists unless force is true, because replacing a key " +
			"is unrecoverable: anything wrapped for the old public key stays wrapped for it.",
		InputSchema: obj(map[string]any{
			"name": map[string]any{
				"type": "string",
				"description": "What to call this device, as a person would: \"laptop\", " +
					"\"studio-mac\". Defaults to the machine's hostname.",
			},
			"force": map[string]any{
				"type": "boolean",
				"description": "Replace an existing key. Destroys the old one. Ask the person " +
					"first — there is no undo and no copy.",
			},
		}),
		Handler: s.generate,
	})

	s.register(Tool{
		Name:     "device_list",
		Title:    "List this machine's device keys",
		ReadOnly: true,
		Description: "List the device keys held on this machine — today at most one. " +
			"Each record carries enrolment_status and unproven; read them before telling anyone " +
			"the key works.",
		InputSchema: obj(map[string]any{}),
		Handler:     s.list,
	})

	s.register(Tool{
		Name:     "device_show",
		Title:    "Show one device key",
		ReadOnly: true,
		Description: "Show one device record: its public key, where the private key lives, and " +
			"whether it is enrolled. Omit device_id to get this machine's.",
		InputSchema: obj(map[string]any{
			"device_id": map[string]any{
				"type":        "string",
				"description": "The device, from device_list. Omit for the one on this machine.",
			},
		}),
		Handler: s.show,
	})

	s.register(Tool{
		Name:        "device_remove",
		Title:       "Remove a device key",
		Destructive: true,
		Description: "Delete a device key and its record from this machine. Unrecoverable: there " +
			"is no escrow and no copy, and Milestone 8 will wrap space keys for a public key that " +
			"would then be gone. Requires the exact device_id, and will not guess.",
		InputSchema: obj(map[string]any{
			"device_id": map[string]any{
				"type":        "string",
				"description": "The device to remove, exactly as device_list prints it.",
			},
		}, "device_id"),
		Handler: s.remove,
	})
}

// decode unmarshals a tool's arguments, refusing unknown fields for the reason
// the schemas say additionalProperties is false: an agent that believes a
// misspelled field was applied has been misled, not merely ignored.
func decode(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("device: the arguments are not valid for this tool: %w", err)
	}
	return nil
}

func (s *Server) generate(args json.RawMessage) (any, error) {
	var in struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	dev, err := s.store.Generate(in.Name, in.Force)
	if err != nil {
		return nil, err
	}
	return map[string]any{"device": device.NewView(dev)}, nil
}

func (s *Server) list(args json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	devices, err := s.store.List()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"devices": device.NewViews(devices),
		// The caveat rides on the LIST as well as on each record, because an
		// empty list is also an answer somebody will act on.
		"authorises": device.NotYetAuthorising,
	}, nil
}

func (s *Server) show(args json.RawMessage) (any, error) {
	var in struct {
		DeviceID string `json:"device_id"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	dev, err := s.store.Get(in.DeviceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"device": device.NewView(dev)}, nil
}

func (s *Server) remove(args json.RawMessage) (any, error) {
	var in struct {
		DeviceID string `json:"device_id"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	dev, err := s.store.Remove(in.DeviceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"removed": device.NewView(dev)}, nil
}
