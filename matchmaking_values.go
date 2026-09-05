package main

import (
	"encoding/json"

	commonpb "npln.nintendo.net/npln-practice/proto/common"
	mmpb "npln.nintendo.net/npln-practice/proto/matchmaking/v1"
)

// gamesyncAttrJSON preserves the typed common.Value representation used by
// NPLN matchmaking tokens. Wonder's captured attributes currently exercise
// integer and string; the remaining scalar cases keep this encoder complete.
func gamesyncAttrJSON(attrs *commonpb.MapValue) string {
	out := map[string]any{}
	for key, value := range attrs.GetFields() {
		out[key] = typedCommonValue(value)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func typedCommonValue(value *commonpb.Value) map[string]any {
	if value == nil {
		return map[string]any{"type": "null", "value": nil}
	}
	switch x := value.GetValueType().(type) {
	case *commonpb.Value_BooleanValue:
		return map[string]any{"type": "boolean", "value": x.BooleanValue}
	case *commonpb.Value_IntegerValue:
		return map[string]any{"type": "integer", "value": x.IntegerValue}
	case *commonpb.Value_FloatValue:
		return map[string]any{"type": "double", "value": x.FloatValue}
	case *commonpb.Value_DoubleValue:
		return map[string]any{"type": "double", "value": x.DoubleValue}
	case *commonpb.Value_StringValue:
		return map[string]any{"type": "string", "value": x.StringValue}
	case *commonpb.Value_BytesValue:
		return map[string]any{"type": "bytes", "value": x.BytesValue}
	case *commonpb.Value_ReferenceValue:
		return map[string]any{"type": "reference", "value": x.ReferenceValue}
	case *commonpb.Value_ArrayValue:
		items := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, item := range x.ArrayValue.GetValues() {
			items = append(items, typedCommonValue(item))
		}
		return map[string]any{"type": "array", "value": items}
	default:
		return map[string]any{"type": "null", "value": nil}
	}
}

func gamesyncLatencyJSON(data *mmpb.LatencyData) string {
	latencies := map[string]any{}
	for region, duration := range data.GetLatencies() {
		latencies[region] = map[string]any{
			"nanos": duration.GetSeconds()*1_000_000_000 + int64(duration.GetNanos()),
		}
	}
	b, err := json.Marshal(map[string]any{"latencies": latencies})
	if err != nil {
		return `{"latencies":{}}`
	}
	return string(b)
}
