package plc

import "strings"

// Electrical units. The PLC's electricity payload has a fixed shape, so these
// are properties of the fields themselves rather than a lookup.
const (
	UnitKilowattHour = "kWh"
	UnitVolt         = "V"
	UnitAmpere       = "A"
	UnitHertz        = "Hz"
	UnitPercent      = "%"
	UnitCelsius      = "°C"
	UnitCubicMetre   = "m³"
)

// meterUnits maps an M-Bus reading key to its unit.
//
// Meters publish bare numbers: the unit is a property of the installation, not
// of the payload, so it has to be recorded somewhere. Here rather than in the
// frontend, so the API and the MCP server report the same thing the panel
// shows. A key that is absent is rendered without a unit — better a bare
// number than a confidently wrong one.
var meterUnits = map[string]string{
	"water_volume_total": UnitCubicMetre,
	"supply_temperature": UnitCelsius,
	// The meter reports heat in kWh. It reads zero because this installation
	// does not meter heat, not because the unit is unknown.
	"heat_total": UnitKilowattHour,
}

// unitFor returns the unit for a meter reading, or an empty string when the
// key is not one we have been told about.
func unitFor(key string) string {
	if u, ok := meterUnits[strings.ToLower(key)]; ok {
		return u
	}
	return ""
}

// unitsFor builds the unit map for one meter's readings, omitting the keys
// with no known unit so a caller can tell "no unit" from "unit is blank".
func unitsFor(readings map[string]float64) map[string]string {
	if len(readings) == 0 {
		return nil
	}
	out := map[string]string{}
	for k := range readings {
		if u := unitFor(k); u != "" {
			out[k] = u
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
