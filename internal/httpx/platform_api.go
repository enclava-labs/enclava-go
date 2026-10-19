package httpx

import "enclava-go/internal/llm"

func (a *App) Provider() llm.Provider {
	return a.provider
}

func (a *App) ProviderReady() bool {
	if a.prober == nil {
		return true
	}
	return a.prober.Ready()
}

func (a *App) ProviderSnapshot() any {
	if a.prober == nil {
		return nil
	}
	return a.prober.Snapshot()
}

