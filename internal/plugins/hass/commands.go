package hass

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Acting on a Home Assistant entity means publishing the right payload to the
// right command topic. Which topic and payload depends on the component and on
// the entity's own configuration, so this file translates a small vocabulary
// of actions — turn_on, set_position, press — into concrete publishes.
//
// Entities whose component mqttview does not know still expose the "publish"
// action, which writes a raw payload to command_topic. That keeps the UI
// useful for a device this code has never heard of.

// Command is a resolved action, ready to publish.
type Command struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// actionsFor lists the actions available for a component given its config.
// Only actions whose topics actually exist are offered, so the UI never shows
// a button that would publish nowhere.
func actionsFor(component string, cfg entityConfig) []string {
	has := func(key string) bool { return cfg.str(key) != "" }
	cmd := has("command_topic")

	var actions []string
	add := func(names ...string) { actions = append(actions, names...) }

	switch component {
	case "switch", "siren", "input_boolean", "automation":
		if cmd {
			add("turn_on", "turn_off", "toggle")
		}

	case "light":
		if cmd {
			add("turn_on", "turn_off", "toggle")
		}
		if has("brightness_command_topic") {
			add("set_brightness")
		}
		if has("effect_command_topic") {
			add("set_effect")
		}
		if has("color_temp_command_topic") {
			add("set_color_temp")
		}

	case "fan":
		if cmd {
			add("turn_on", "turn_off", "toggle")
		}
		if has("percentage_command_topic") {
			add("set_percentage")
		}
		if has("preset_mode_command_topic") {
			add("set_preset")
		}
		if has("oscillation_command_topic") {
			add("set_oscillation")
		}

	case "lock":
		if cmd {
			add("lock", "unlock")
			if has("payload_open") {
				add("open")
			}
		}

	case "cover", "valve":
		if cmd {
			add("open", "close", "stop")
		}
		if has("set_position_topic") {
			add("set_position")
		}
		if has("tilt_command_topic") {
			add("set_tilt")
		}

	case "button", "scene":
		if cmd {
			add("press")
		}

	case "number", "text", "select":
		if cmd {
			add("set")
		}

	case "climate", "water_heater":
		if has("temperature_command_topic") {
			add("set_temperature")
		}
		if has("mode_command_topic") {
			add("set_mode")
		}
		if has("preset_mode_command_topic") {
			add("set_preset")
		}
		if has("fan_mode_command_topic") {
			add("set_fan_mode")
		}
		if has("swing_mode_command_topic") {
			add("set_swing_mode")
		}
		if has("power_command_topic") {
			add("turn_on", "turn_off")
		}

	case "humidifier":
		if cmd {
			add("turn_on", "turn_off")
		}
		if has("target_humidity_command_topic") {
			add("set_humidity")
		}
		if has("mode_command_topic") {
			add("set_mode")
		}

	case "vacuum":
		if cmd {
			add("start", "stop", "pause", "return_to_base", "locate", "clean_spot")
		}

	case "update":
		if cmd {
			add("install")
		}

	case "alarm_control_panel":
		if cmd {
			add("arm_home", "arm_away", "arm_night", "disarm")
		}
	}

	if cmd {
		// Always available as an escape hatch, including for components this
		// switch does not cover.
		add("publish")
	}
	return dedupe(actions)
}

// BuildCommand turns an action plus an optional value into a publish.
func BuildCommand(e *Entity, action, value string) (Command, error) {
	cfg := entityConfig(e.Config)
	qos := byte(0)
	if q, ok := cfg.number("qos"); ok && q >= 0 && q <= 2 {
		qos = byte(q)
	}
	retain := cfg.boolean("retain", false)

	topic, payload, err := resolveCommand(e, cfg, action, value)
	if err != nil {
		return Command{}, err
	}
	if topic == "" {
		return Command{}, fmt.Errorf("hass: entity %q has no topic for action %q", e.Name, action)
	}
	return Command{Topic: topic, Payload: payload, QoS: qos, Retain: retain}, nil
}

func resolveCommand(e *Entity, cfg entityConfig, action, value string) (string, []byte, error) {
	cmdTopic := cfg.str("command_topic")

	switch action {
	case "publish":
		return cmdTopic, []byte(value), nil

	case "turn_on":
		if t := cfg.str("power_command_topic"); t != "" && e.Component == "climate" {
			return t, []byte(cfg.strDefault("payload_on", "ON")), nil
		}
		return cmdTopic, []byte(cfg.strDefault("payload_on", "ON")), nil

	case "turn_off":
		if t := cfg.str("power_command_topic"); t != "" && e.Component == "climate" {
			return t, []byte(cfg.strDefault("payload_off", "OFF")), nil
		}
		return cmdTopic, []byte(cfg.strDefault("payload_off", "OFF")), nil

	case "toggle":
		// MQTT has no toggle; derive it from the last known state so the UI
		// button does the obvious thing.
		on := cfg.strDefault("payload_on", "ON")
		off := cfg.strDefault("payload_off", "OFF")
		stateOn := cfg.strDefault("state_on", on)
		if e.State != nil && toString(e.State.Value) == stateOn {
			return cmdTopic, []byte(off), nil
		}
		return cmdTopic, []byte(on), nil

	case "press":
		if e.Component == "scene" {
			return cmdTopic, []byte(cfg.strDefault("payload_on", "ON")), nil
		}
		return cmdTopic, []byte(cfg.strDefault("payload_press", "PRESS")), nil

	case "install":
		return cmdTopic, []byte(cfg.strDefault("payload_install", "install")), nil

	case "lock":
		return cmdTopic, []byte(cfg.strDefault("payload_lock", "LOCK")), nil
	case "unlock":
		return cmdTopic, []byte(cfg.strDefault("payload_unlock", "UNLOCK")), nil

	case "open":
		return cmdTopic, []byte(cfg.strDefault("payload_open", "OPEN")), nil
	case "close":
		return cmdTopic, []byte(cfg.strDefault("payload_close", "CLOSE")), nil
	case "stop":
		return cmdTopic, []byte(cfg.strDefault("payload_stop", "STOP")), nil

	case "start":
		return cmdTopic, []byte(cfg.strDefault("payload_start", "start")), nil
	case "pause":
		return cmdTopic, []byte(cfg.strDefault("payload_pause", "pause")), nil
	case "return_to_base":
		return cmdTopic, []byte(cfg.strDefault("payload_return_to_base", "return_to_base")), nil
	case "locate":
		return cmdTopic, []byte(cfg.strDefault("payload_locate", "locate")), nil
	case "clean_spot":
		return cmdTopic, []byte(cfg.strDefault("payload_clean_spot", "clean_spot")), nil

	case "arm_home":
		return cmdTopic, []byte(cfg.strDefault("payload_arm_home", "ARM_HOME")), nil
	case "arm_away":
		return cmdTopic, []byte(cfg.strDefault("payload_arm_away", "ARM_AWAY")), nil
	case "arm_night":
		return cmdTopic, []byte(cfg.strDefault("payload_arm_night", "ARM_NIGHT")), nil
	case "disarm":
		return cmdTopic, []byte(cfg.strDefault("payload_disarm", "DISARM")), nil

	case "set":
		if err := validateSetValue(e, cfg, value); err != nil {
			return "", nil, err
		}
		return cmdTopic, []byte(value), nil

	case "set_brightness":
		scale := 255.0
		if s, ok := cfg.number("brightness_scale"); ok && s > 0 {
			scale = s
		}
		n, err := parseNumber(value)
		if err != nil {
			return "", nil, err
		}
		if n < 0 || n > scale {
			return "", nil, fmt.Errorf("hass: brightness must be between 0 and %g", scale)
		}
		return cfg.str("brightness_command_topic"), []byte(formatNumber(n)), nil

	case "set_percentage":
		n, err := parsePercent(value)
		if err != nil {
			return "", nil, err
		}
		return cfg.str("percentage_command_topic"), []byte(formatNumber(n)), nil

	case "set_position":
		n, err := parsePercent(value)
		if err != nil {
			return "", nil, err
		}
		return cfg.str("set_position_topic"), []byte(formatNumber(n)), nil

	case "set_tilt":
		n, err := parsePercent(value)
		if err != nil {
			return "", nil, err
		}
		return cfg.str("tilt_command_topic"), []byte(formatNumber(n)), nil

	case "set_temperature":
		n, err := parseNumber(value)
		if err != nil {
			return "", nil, err
		}
		if err := checkRange(cfg, "min_temp", "max_temp", n, "temperature"); err != nil {
			return "", nil, err
		}
		return cfg.str("temperature_command_topic"), []byte(formatNumber(n)), nil

	case "set_humidity":
		n, err := parsePercent(value)
		if err != nil {
			return "", nil, err
		}
		return cfg.str("target_humidity_command_topic"), []byte(formatNumber(n)), nil

	case "set_color_temp":
		n, err := parseNumber(value)
		if err != nil {
			return "", nil, err
		}
		return cfg.str("color_temp_command_topic"), []byte(formatNumber(n)), nil

	case "set_mode":
		if err := checkOption(cfg, "modes", value); err != nil {
			return "", nil, err
		}
		return cfg.str("mode_command_topic"), []byte(value), nil

	case "set_preset":
		if err := checkOption(cfg, "preset_modes", value); err != nil {
			return "", nil, err
		}
		return cfg.str("preset_mode_command_topic"), []byte(value), nil

	case "set_fan_mode":
		if err := checkOption(cfg, "fan_modes", value); err != nil {
			return "", nil, err
		}
		return cfg.str("fan_mode_command_topic"), []byte(value), nil

	case "set_swing_mode":
		if err := checkOption(cfg, "swing_modes", value); err != nil {
			return "", nil, err
		}
		return cfg.str("swing_mode_command_topic"), []byte(value), nil

	case "set_effect":
		if err := checkOption(cfg, "effect_list", value); err != nil {
			return "", nil, err
		}
		return cfg.str("effect_command_topic"), []byte(value), nil

	case "set_oscillation":
		on := cfg.strDefault("payload_oscillation_on", "oscillate_on")
		off := cfg.strDefault("payload_oscillation_off", "oscillate_off")
		if isTruthy(value) {
			return cfg.str("oscillation_command_topic"), []byte(on), nil
		}
		return cfg.str("oscillation_command_topic"), []byte(off), nil

	default:
		return "", nil, fmt.Errorf("hass: unknown action %q", action)
	}
}

// validateSetValue keeps a "set" from publishing something the device has
// already told us it will reject.
func validateSetValue(e *Entity, cfg entityConfig, value string) error {
	switch e.Component {
	case "number":
		n, err := parseNumber(value)
		if err != nil {
			return err
		}
		return checkRange(cfg, "min", "max", n, "value")
	case "select":
		return checkOption(cfg, "options", value)
	case "text":
		if maxLen, ok := cfg.number("max"); ok && float64(len(value)) > maxLen {
			return fmt.Errorf("hass: text must be at most %g characters", maxLen)
		}
		return nil
	}
	return nil
}

func checkRange(cfg entityConfig, minKey, maxKey string, n float64, label string) error {
	if minV, ok := cfg.number(minKey); ok && n < minV {
		return fmt.Errorf("hass: %s must be at least %g", label, minV)
	}
	if maxV, ok := cfg.number(maxKey); ok && n > maxV {
		return fmt.Errorf("hass: %s must be at most %g", label, maxV)
	}
	return nil
}

func checkOption(cfg entityConfig, key, value string) error {
	options := cfg.strings(key)
	if len(options) == 0 {
		return nil // the device did not constrain the choice
	}
	for _, o := range options {
		if o == value {
			return nil
		}
	}
	return fmt.Errorf("hass: %q is not one of %s", value, strings.Join(options, ", "))
}

func parseNumber(value string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("hass: %q is not a number", value)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("hass: %q is not a usable number", value)
	}
	return n, nil
}

func parsePercent(value string) (float64, error) {
	n, err := parseNumber(value)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("hass: value must be between 0 and 100")
	}
	return n, nil
}

func formatNumber(n float64) string {
	if n == math.Trunc(n) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes", "oscillate_on":
		return true
	}
	return false
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
