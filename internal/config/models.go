package config

import "strings"

const AdvertisedMaxContextTokens = 128000

type ModelInfo struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextLength int    `json:"context_length"`
	Permission    []any  `json:"permission,omitempty"`
}

// 仅公开这三个模型加上必要的兼容隐藏/测试模型
var GLModels = []ModelInfo{
	{ID: "deepseek-v3.2", Object: "model", Created: 1718000000, OwnedBy: "generalcompute", ContextLength: 128000, Permission: []any{}},
	{ID: "deepseek-v3.1", Object: "model", Created: 1718000000, OwnedBy: "generalcompute", ContextLength: 128000, Permission: []any{}},
	{ID: "minimax-m2.7", Object: "model", Created: 1718000000, OwnedBy: "generalcompute", ContextLength: 128000, Permission: []any{}},
	{ID: "gemma-4-31B-it", Object: "model", Created: 1718000000, OwnedBy: "generalcompute", ContextLength: 128000, Permission: []any{}},
	{ID: "gpt-oss-120b", Object: "model", Created: 1718000000, OwnedBy: "generalcompute", ContextLength: 128000, Permission: []any{}},
}

func ResolveModel(requested string) (string, bool) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	switch requested {
	case "deepseek-v3.2", "deepseek-v3.1", "minimax-m2.7", "gpt-oss-120b", "gemma-4-31B-it":
		return requested, true
	case "minimax2.7", "minimax-2.7", "minimax-m2-7":
		return "minimax-m2.7", true
	default:
		return "", false
	}
}

func OpenAIModelsResponse() map[string]any {
	return map[string]any{"object": "list", "data": GLModels}
}

func OpenAIModelByID(id string) (ModelInfo, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, model := range GLModels {
		if model.ID == id {
			return model, true
		}
	}
	return ModelInfo{}, false
}

// ================= 保留旧兼容接口防止编译报错 =================

func GetModelConfig(model string) (thinking bool, search bool, ok bool) {
	_, ok = ResolveModel(model)
	return true, false, ok
}

func GetModelType(model string) (modelType string, ok bool) {
	_, ok = ResolveModel(model)
	if ok {
		return "default", true
	}
	return "", false
}

func UpstreamDeepSeekSKU(resolvedModel string) string {
	return resolvedModel
}

func UpstreamSafeModelType(modelType string) string {
	return "default"
}

func IsNoThinkingModel(model string) bool {
	return false
}
