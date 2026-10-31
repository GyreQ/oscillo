package collector

import "encoding/json"

// unmarshalLoose 宽松反序列化的统一入口（集中 import，便于将来换更快的解码器）。
func unmarshalLoose(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
