package store_test

import (
	"context"
	"testing"

	"github.com/crashcartapp/crashcart/internal/store"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestPushDeviceUpsertAndSubscribe(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	key1, err := store.CreateAPIKey(ctx, st.Pool, "k1", []byte("h1"), "cc_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	key2, err := store.CreateAPIKey(ctx, st.Pool, "k2", []byte("h2"), "cc_2", nil)
	if err != nil {
		t.Fatal(err)
	}

	d, err := store.UpsertPushDevice(ctx, st.Pool, key1.ID, "tok-1", "ios")
	if err != nil || d.Platform != "ios" {
		t.Fatalf("upsert: %v %v", d, err)
	}

	// Same token, different key and platform (reinstall on a different
	// account, or just a refreshed registration): updates the row in
	// place rather than creating a duplicate.
	d2, err := store.UpsertPushDevice(ctx, st.Pool, key2.ID, "tok-1", "android")
	if err != nil || d2.ID != d.ID || d2.Platform != "android" {
		t.Fatalf("upsert by token must update, not duplicate: %v %v", d2, err)
	}

	// Ownership followed the upsert to key2: key1 can no longer subscribe
	// or delete this device.
	if ok, err := store.SubscribePush(ctx, st.Pool, key1.ID, d.ID, 1); err != nil || ok {
		t.Errorf("subscribe by the old owner: ok=%v err=%v", ok, err)
	}
	if ok, err := store.SubscribePush(ctx, st.Pool, key2.ID, d.ID, 1); err != nil || !ok {
		t.Fatalf("subscribe by the current owner: ok=%v err=%v", ok, err)
	}
	if ok, err := store.SubscribePush(ctx, st.Pool, key2.ID, d.ID, 1); err != nil || !ok {
		t.Errorf("subscribing twice must stay idempotent: ok=%v err=%v", ok, err)
	}

	subs, err := store.ListPushSubscribers(ctx, st.Pool, 1)
	if err != nil || len(subs) != 1 || subs[0].ID != d.ID {
		t.Fatalf("subscribers: %v %v", subs, err)
	}

	if n, err := store.UnsubscribePush(ctx, st.Pool, key1.ID, d.ID, 1); err != nil || n != 0 {
		t.Errorf("unsubscribe by the old owner: n=%d err=%v", n, err)
	}
	if n, err := store.UnsubscribePush(ctx, st.Pool, key2.ID, d.ID, 1); err != nil || n != 1 {
		t.Fatalf("unsubscribe: n=%d err=%v", n, err)
	}
	if subs, err := store.ListPushSubscribers(ctx, st.Pool, 1); err != nil || len(subs) != 0 {
		t.Errorf("subscribers after unsubscribe: %v %v", subs, err)
	}

	if n, err := store.DeletePushDevice(ctx, st.Pool, key1.ID, d.ID); err != nil || n != 0 {
		t.Errorf("delete by the old owner: n=%d err=%v", n, err)
	}
	if n, err := store.DeletePushDevice(ctx, st.Pool, key2.ID, d.ID); err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
}

// TestPushDeviceCascadesOnKeyDelete: revoking (deleting) an API key must
// stop push notifications for every device it registered — this is the
// only "lost phone" story the mobile apps rely on.
func TestPushDeviceCascadesOnKeyDelete(t *testing.T) {
	st := testdb.New(t)
	testdb.Projects(t, st, 1)
	ctx := context.Background()
	key, err := store.CreateAPIKey(ctx, st.Pool, "k", []byte("h"), "cc_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.UpsertPushDevice(ctx, st.Pool, key.ID, "tok-2", "ios")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SubscribePush(ctx, st.Pool, key.ID, d.ID, 1); err != nil || !ok {
		t.Fatalf("subscribe: ok=%v err=%v", ok, err)
	}

	if _, err := st.Pool.Exec(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM push_devices WHERE id = $1", d.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("push_devices row survived its api_key's deletion")
	}
	if subs, err := store.ListPushSubscribers(ctx, st.Pool, 1); err != nil || len(subs) != 0 {
		t.Errorf("subscription survived the device's deletion: %v %v", subs, err)
	}
}
