package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSubscriptionRoutingEnablesAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset-first"}})
	if state.strategy != "soonest-reset-first" || !state.sessionAffinity {
		t.Fatalf("routing state = %+v", state)
	}
	selector := newRoutingSelector(state)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector = %T; want session affinity", selector)
	}
	defer affinity.Stop()
	m := coreauth.NewManager(nil, selector, nil)
	if !m.UsesSubscriptionRouting() {
		t.Fatal("affinity wrapper hid subscription routing")
	}
}
