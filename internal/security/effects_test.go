package security

import (
	"context"
	"testing"
)

func TestEffectGrantIsCopiedFromContext(t *testing.T) {
	ctx := WithEffectGrant(context.Background(), EffectRead)
	grant, ok := EffectGrantFromContext(ctx)
	if !ok {
		t.Fatal("effect grant missing")
	}
	grant[EffectDeliveryWrite] = struct{}{}
	again, _ := EffectGrantFromContext(ctx)
	if _, ok := again[EffectDeliveryWrite]; ok {
		t.Fatal("caller mutated context effect grant")
	}
}
