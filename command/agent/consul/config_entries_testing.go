package consul

import (
	"sync"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-hclog"
)

var _ ConfigAPI = (*MockConfigsAPI)(nil)

type MockConfigsAPI struct {
	logger hclog.Logger

	lock  sync.Mutex
	state struct {
		error         error
		configEntries map[string]*api.ConfigEntry
	}
}

func (m MockConfigsAPI) Set(entry api.ConfigEntry, w *api.WriteOptions) (bool, *api.WriteMeta, error) {
	// todo
	return false, nil, nil
}

func (m MockConfigsAPI) Delete(kind, name string, w *api.WriteOptions) (*api.WriteMeta, error) {
	// todo
	return nil, nil
}

func NewMockConfigsAPI(l hclog.Logger) *MockConfigsAPI {
	return &MockConfigsAPI{
		logger: l.Named("mock_consul"),
		state: struct {
			error         error
			configEntries map[string]*api.ConfigEntry
		}{configEntries: make(map[string]*api.ConfigEntry)},
	}
}
