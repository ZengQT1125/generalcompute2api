package shared

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
)

type ModelsHandler struct {
	Store ConfigReader
	Auth  AuthResolver
}

func (h *ModelsHandler) ListModels(w http.ResponseWriter, r *http.Request) {
	if h.Auth != nil {
		if _, err := h.Auth.DetermineCaller(r); err != nil {
			status := http.StatusUnauthorized
			if err == auth.ErrNoAccount {
				status = http.StatusTooManyRequests
			}
			WriteOpenAIError(w, status, err.Error())
			return
		}
	}
	WriteJSON(w, http.StatusOK, config.OpenAIModelsResponse())
}

func (h *ModelsHandler) GetModel(w http.ResponseWriter, r *http.Request) {
	if h.Auth != nil {
		if _, err := h.Auth.DetermineCaller(r); err != nil {
			status := http.StatusUnauthorized
			if err == auth.ErrNoAccount {
				status = http.StatusTooManyRequests
			}
			WriteOpenAIError(w, status, err.Error())
			return
		}
	}
	modelID := strings.TrimSpace(chi.URLParam(r, "model_id"))
	model, ok := config.OpenAIModelByID(modelID)
	if !ok {
		WriteOpenAIError(w, http.StatusNotFound, "Model not found.")
		return
	}
	WriteJSON(w, http.StatusOK, model)
}

