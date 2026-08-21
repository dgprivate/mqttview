package plc

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CommandRequest is one command aimed at the PLC.
type CommandRequest struct {
	ConnectionID string   `json:"connectionId"`
	Target       string   `json:"target"`
	Command      string   `json:"command"`
	Address      int      `json:"address,omitempty"`
	Params       []string `json:"params,omitempty"`
}

// commandSpec describes a command this plugin is willing to send.
//
// This is an allow-list rather than a deny-list, and deliberately so. The PLC
// accepts a much larger vocabulary than appears here: it can arm and disarm the
// alarm, trip a panic, drive the water valve, flip the direct-to-Home-Assistant
// mode and wipe its persistent state. None of those belong behind a button in a
// topic browser, and an allow-list means a command added to the PLC tomorrow
// cannot quietly become reachable from here.
type commandSpec struct {
	Target      string `json:"target"`
	Command     string `json:"command"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// MinAddress and MaxAddress bound the address; both zero means the command
	// takes no address.
	MinAddress int `json:"minAddress,omitempty"`
	MaxAddress int `json:"maxAddress,omitempty"`
	// Param names the single parameter the command takes, if any.
	Param string `json:"param,omitempty"`
	// Tier decides which setting has to be on before this may be sent.
	Tier commandTier `json:"tier"`
}

// commandTier separates commands that are trivially reversible from ones that
// drive whatever happens to be wired to an output.
type commandTier string

const (
	// tierLight covers the DALI bus: worst case a light is the wrong
	// brightness, and the next command puts it back.
	tierLight commandTier = "light"
	// tierOutput covers raw digital outputs. Among the eighty are a door
	// lock, a water valve and a cooktop relay, so it has its own switch and
	// its own warning.
	tierOutput commandTier = "output"
)

// commandCatalogue is every command the plugin will send, and the only one.
var commandCatalogue = []commandSpec{
	{
		Target: "dali", Command: "on", Label: "On",
		Description: "Recall the ballast's maximum level.",
		MinAddress:  1, MaxAddress: 64, Tier: tierLight,
	},
	{
		Target: "dali", Command: "off", Label: "Off",
		Description: "Switch the ballast off.",
		MinAddress:  1, MaxAddress: 64, Tier: tierLight,
	},
	{
		Target: "dali", Command: "arc", Label: "Set level",
		Description: "Set the direct arc power level, 0 to 254.",
		MinAddress:  1, MaxAddress: 64, Param: "level", Tier: tierLight,
	},
	{
		Target: "dali", Command: "query_actual_level", Label: "Query level",
		Description: "Ask the ballast what level it is actually at.",
		MinAddress:  1, MaxAddress: 64, Tier: tierLight,
	},
	{
		Target: "dali", Command: "refresh", Label: "Refresh all DALI",
		Description: "Re-read the state of every ballast on the bus.",
		Tier:        tierLight,
	},
	{
		Target: "system", Command: "refresh", Label: "Refresh state",
		Description: "Ask the PLC to republish its current state.",
		Tier:        tierLight,
	},
	{
		Target: "digital", Command: "set", Label: "Set output",
		Description: "Drive a digital output true or false. Check what is wired to the address first.",
		MinAddress:  1, MaxAddress: 80, Param: "value", Tier: tierOutput,
	},
}

// lookupCommand finds a spec by target and command.
func lookupCommand(target, command string) (commandSpec, bool) {
	for _, s := range commandCatalogue {
		if s.Target == target && s.Command == command {
			return s, true
		}
	}
	return commandSpec{}, false
}

// commandPayload is the wire envelope the PLC's FB_MqttCommandProcessor parses.
type commandPayload struct {
	Target  string   `json:"target"`
	Command string   `json:"command"`
	Address int      `json:"address,omitempty"`
	Params  []string `json:"params,omitempty"`
}

// buildCommand validates a request against the catalogue and renders the
// payload. It returns the spec so the caller can check the tier.
func buildCommand(req CommandRequest) (commandSpec, []byte, error) {
	spec, ok := lookupCommand(strings.TrimSpace(req.Target), strings.TrimSpace(req.Command))
	if !ok {
		return commandSpec{}, nil, fmt.Errorf("plc: %q/%q is not a command this plugin will send", req.Target, req.Command)
	}

	payload := commandPayload{Target: spec.Target, Command: spec.Command}

	if spec.MaxAddress > 0 {
		if req.Address < spec.MinAddress || req.Address > spec.MaxAddress {
			return spec, nil, fmt.Errorf("plc: address must be between %d and %d for %s/%s",
				spec.MinAddress, spec.MaxAddress, spec.Target, spec.Command)
		}
		payload.Address = req.Address
	} else if req.Address != 0 {
		return spec, nil, fmt.Errorf("plc: %s/%s takes no address", spec.Target, spec.Command)
	}

	switch spec.Param {
	case "":
		if len(req.Params) > 0 {
			return spec, nil, fmt.Errorf("plc: %s/%s takes no parameters", spec.Target, spec.Command)
		}
	case "level":
		value, err := singleParam(req.Params, spec)
		if err != nil {
			return spec, nil, err
		}
		level, err := strconv.Atoi(value)
		if err != nil || level < 0 || level > daliMaxLevel {
			return spec, nil, fmt.Errorf("plc: level must be a whole number between 0 and %d", daliMaxLevel)
		}
		payload.Params = []string{strconv.Itoa(level)}
	case "value":
		value, err := singleParam(req.Params, spec)
		if err != nil {
			return spec, nil, err
		}
		// The PLC accepts several spellings; normalising here means the UI and
		// the API can send whatever is natural and the PLC sees one form.
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "on":
			payload.Params = []string{"true"}
		case "false", "0", "off":
			payload.Params = []string{"false"}
		default:
			return spec, nil, fmt.Errorf("plc: value must be true or false, got %q", value)
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return spec, nil, err
	}
	return spec, body, nil
}

func singleParam(params []string, spec commandSpec) (string, error) {
	if len(params) != 1 {
		return "", fmt.Errorf("plc: %s/%s needs exactly one parameter (%s)", spec.Target, spec.Command, spec.Param)
	}
	return params[0], nil
}
