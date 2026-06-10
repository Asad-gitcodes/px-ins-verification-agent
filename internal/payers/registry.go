package payers

import "fmt"

type Registry struct {
	// Registry lets jobmgr ask for a payerUrl without importing each
	// concrete adapter package.
	adapters []Adapter
}

func NewRegistry() *Registry {
	return &Registry{
		adapters: []Adapter{},
	}
}

func (r *Registry) Register(adapter Adapter) {
	r.adapters = append(r.adapters, adapter)
}

func (r *Registry) GetForPayerURL(payerURL string) (Adapter, error) {
	for _, adapter := range r.adapters {
		if adapter.Supports(payerURL) {
			return adapter, nil
		}
	}

	return nil, fmt.Errorf("payer adapter not registered for payerUrl: %s", payerURL)
}
