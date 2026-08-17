package utils

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func setupStore(t *testing.T) {
	t.Helper()
	tmp := t.TempDir() + "/"
	prev := CONFIGFOLDER
	CONFIGFOLDER = tmp
	if err := InitStore(); err != nil {
		t.Fatal("InitStore:", err)
	}
	t.Cleanup(func() {
		CloseStore()
		CONFIGFOLDER = prev
	})
}

func TestStoreUserCRUD(t *testing.T) {
	setupStore(t)

	created := time.Date(2024, 5, 1, 12, 30, 45, 123456789, time.UTC)
	err := CreateUser(User{
		Nickname:  "alice",
		Password:  "hashed-secret",
		Email:     "alice@example.com",
		Role:      ADMIN,
		CreatedAt: created,
		// RegisterKeyExp / LastLogin left zero on purpose
	})
	if err != nil {
		t.Fatal("CreateUser:", err)
	}

	u, err := GetUser("alice")
	if err != nil {
		t.Fatal("GetUser:", err)
	}
	if u.Password != "hashed-secret" || u.Email != "alice@example.com" || u.Role != ADMIN {
		t.Fatalf("round-trip mismatch: %+v", u)
	}
	if !u.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt mismatch: %v != %v", u.CreatedAt, created)
	}
	if !u.RegisterKeyExp.IsZero() || !u.LastLogin.IsZero() {
		t.Fatalf("zero times must round-trip as zero: %+v", u)
	}

	exp := time.Now().Add(time.Hour).UTC()
	if err := UpdateUser("alice", map[string]interface{}{
		"RegisterKey":    "key123",
		"RegisterKeyExp": exp,
		"Was2FAVerified": true,
		"PasswordCycle":  3,
	}); err != nil {
		t.Fatal("UpdateUser:", err)
	}
	u, _ = GetUser("alice")
	if u.RegisterKey != "key123" || !u.RegisterKeyExp.Equal(exp) || !u.Was2FAVerified || u.PasswordCycle != 3 {
		t.Fatalf("update mismatch: %+v", u)
	}

	// zero a time back out
	if err := UpdateUser("alice", map[string]interface{}{"RegisterKeyExp": time.Time{}}); err != nil {
		t.Fatal("UpdateUser zero time:", err)
	}
	u, _ = GetUser("alice")
	if !u.RegisterKeyExp.IsZero() {
		t.Fatal("zeroed time did not round-trip as zero")
	}

	count, err := CountUsers()
	if err != nil || count != 1 {
		t.Fatalf("CountUsers = %d, %v", count, err)
	}

	if err := DeleteUser("alice"); err != nil {
		t.Fatal("DeleteUser:", err)
	}
	if _, err := GetUser("alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStoreErrNotFound(t *testing.T) {
	setupStore(t)

	if u, err := GetUser("ghost"); !errors.Is(err, ErrNotFound) || u.Nickname != "" || u.Password != "" {
		t.Fatalf("GetUser: want zero User + ErrNotFound, got %+v, %v", u, err)
	}
	if d, err := GetDeviceByName("ghost", false); !errors.Is(err, ErrNotFound) || d.DeviceName != "" || d.APIKey != "" {
		t.Fatalf("GetDeviceByName: want zero device + ErrNotFound, got %+v, %v", d, err)
	}
	if d, err := GetDeviceByIP("10.9.9.9"); !errors.Is(err, ErrNotFound) || d.DeviceName != "" {
		t.Fatalf("GetDeviceByIP: want zero device + ErrNotFound, got %+v, %v", d, err)
	}
}

func TestStoreDeviceCRUDAndTags(t *testing.T) {
	setupStore(t)

	err := CreateDevice(ConstellationDevice{
		DeviceName: "node1",
		Nickname:   "alice",
		IP:         "192.168.201.2",
		APIKey:     "secret-api-key",
		Tags:       []string{"gpu", "eu-west"},
	})
	if err != nil {
		t.Fatal("CreateDevice:", err)
	}

	d, err := GetDeviceByName("node1", true)
	if err != nil {
		t.Fatal("GetDeviceByName:", err)
	}
	if d.APIKey != "secret-api-key" || len(d.Tags) != 2 || d.Tags[0] != "gpu" || d.Tags[1] != "eu-west" {
		t.Fatalf("device round-trip mismatch: %+v", d)
	}

	// no tags round-trips as nil
	if err := CreateDevice(ConstellationDevice{DeviceName: "node2", IP: "192.168.201.3"}); err != nil {
		t.Fatal("CreateDevice node2:", err)
	}
	d2, _ := GetDeviceByName("node2", true)
	if d2.Tags != nil {
		t.Fatalf("empty tags should round-trip as nil, got %v", d2.Tags)
	}

	if err := UpdateDevices(
		map[string]interface{}{"DeviceName": "node1", "Blocked": false},
		map[string]interface{}{"Tags": []string{"cpu"}, "IsRelay": true},
	); err != nil {
		t.Fatal("UpdateDevices:", err)
	}
	d, _ = GetDeviceByName("node1", true)
	if !d.IsRelay || len(d.Tags) != 1 || d.Tags[0] != "cpu" {
		t.Fatalf("device update mismatch: %+v", d)
	}

	devices, err := FindDevices(map[string]interface{}{"Nickname": "alice"})
	if err != nil || len(devices) != 1 {
		t.Fatalf("FindDevices = %v, %v", devices, err)
	}

	if err := DeleteDevices(map[string]interface{}{"DeviceName": "node2"}); err != nil {
		t.Fatal("DeleteDevices:", err)
	}
	all, _ := ListDevices(true)
	if len(all) != 1 {
		t.Fatalf("expected 1 device after delete, got %d", len(all))
	}
}

func TestStorePartialUniqueIndexes(t *testing.T) {
	setupStore(t)

	if err := CreateDevice(ConstellationDevice{DeviceName: "dup", IP: "192.168.201.10"}); err != nil {
		t.Fatal("CreateDevice active:", err)
	}

	// a blocked device may share name AND ip with a live replacement
	if err := CreateDevice(ConstellationDevice{DeviceName: "dup", IP: "192.168.201.10", Blocked: true}); err != nil {
		t.Fatal("blocked duplicate must be allowed:", err)
	}

	// an active duplicate name is rejected by the partial index
	err := CreateDevice(ConstellationDevice{DeviceName: "dup", IP: "192.168.201.11"})
	var ec *ErrConstraint
	if !errors.As(err, &ec) {
		t.Fatalf("expected ErrConstraint for active dup name, got %v", err)
	}
	if ec.Table != "devices" {
		t.Fatalf("ErrConstraint.Table = %q", ec.Table)
	}

	// an active duplicate ip is rejected too
	err = CreateDevice(ConstellationDevice{DeviceName: "other", IP: "192.168.201.10"})
	if !errors.As(err, &ec) {
		t.Fatalf("expected ErrConstraint for active dup ip, got %v", err)
	}

	// blocking the live one frees the name for a replacement
	if err := UpdateDevices(map[string]interface{}{"DeviceName": "dup"}, map[string]interface{}{"Blocked": true}); err != nil {
		t.Fatal("block:", err)
	}
	if err := CreateDevice(ConstellationDevice{DeviceName: "dup", IP: "192.168.201.12"}); err != nil {
		t.Fatal("replacement after block:", err)
	}
}

func TestStoreConcurrentReadWrite(t *testing.T) {
	setupStore(t)

	const writers = 4
	const perWriter = 20
	done := make(chan struct{})
	var wg sync.WaitGroup

	// concurrent readers
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					ListUsers("")
					GetUser("w0-5")
					CountDevices(map[string]interface{}{})
					GetLastAppliedSeq()
				}
			}
		}()
	}

	var werr sync.Map
	var wwg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wwg.Add(1)
		go func(wi int) {
			defer wwg.Done()
			for i := 0; i < perWriter; i++ {
				nick := fmt.Sprintf("w%d-%d", wi, i)
				if err := CreateUser(User{Nickname: nick, CreatedAt: time.Now()}); err != nil {
					werr.Store(nick, err)
				}
			}
		}(wi)
	}
	wwg.Wait()
	close(done)
	wg.Wait()

	werr.Range(func(k, v interface{}) bool {
		t.Errorf("write %v failed: %v", k, v)
		return true
	})

	count, err := CountUsers()
	if err != nil || count != writers*perWriter {
		t.Fatalf("CountUsers = %d, %v (want %d)", count, err, writers*perWriter)
	}
}

func TestStoreLogicalDumpRoundTrip(t *testing.T) {
	setupStore(t)

	CreateUser(User{Nickname: "bob", Password: "hash", Role: USER})
	CreateDevice(ConstellationDevice{DeviceName: "d1", IP: "192.168.201.5", APIKey: "k", Tags: []string{"t"}})
	CreateDevice(ConstellationDevice{DeviceName: "d1", IP: "192.168.201.5", Blocked: true})

	dump, err := BuildLogicalDump()
	if err != nil {
		t.Fatal("BuildLogicalDump:", err)
	}

	if err := ApplyLogicalDump(dump, 3, 4242); err != nil {
		t.Fatal("ApplyLogicalDump:", err)
	}

	if epoch, seq := GetOplogEpoch(), GetLastAppliedSeq(); epoch != 3 || seq != 4242 {
		t.Fatalf("op-log position not adopted: epoch %d seq %d", epoch, seq)
	}

	u, err := GetUser("bob")
	if err != nil || u.Password != "hash" {
		t.Fatalf("user lost in dump round-trip: %+v, %v", u, err)
	}
	devices, _ := ListDevices(true)
	if len(devices) != 2 {
		t.Fatalf("devices lost in dump round-trip: %d", len(devices))
	}

	// canonical: same content must serialize identically
	dump2, err := BuildLogicalDump()
	if err != nil {
		t.Fatal("BuildLogicalDump 2:", err)
	}
	if string(dump) != string(dump2) {
		t.Fatal("dump is not canonical across rebuilds")
	}
}

func TestStoreOplogPosition(t *testing.T) {
	setupStore(t)

	if GetOplogEpoch() != 1 || GetLastAppliedSeq() != 0 {
		t.Fatal("fresh store should start at epoch 1, seq 0")
	}

	// applying an op commits its sequence in the same tx as the data
	m := Mutation{Table: "users", Op: "insert", Doc: User{Nickname: "seqbob"}}
	if err := ApplyOpTx(m, 7, nil); err != nil {
		t.Fatal("ApplyOpTx:", err)
	}
	if seq := GetLastAppliedSeq(); seq != 7 {
		t.Fatalf("seq not committed with the data: %d", seq)
	}
	if _, err := GetUser("seqbob"); err != nil {
		t.Fatal("data not committed with the seq:", err)
	}

	// a rejected op still consumes its sequence
	if err := CommitOplogSeq(8); err != nil {
		t.Fatal("CommitOplogSeq:", err)
	}
	if seq := GetLastAppliedSeq(); seq != 8 {
		t.Fatalf("rejected op did not consume its seq: %d", seq)
	}

	// a direct write leaves the log position untouched
	if err := CommitMutationDirect(Mutation{Table: "users", Op: "insert", Doc: User{Nickname: "direct"}}, nil); err != nil {
		t.Fatal("CommitMutationDirect:", err)
	}
	if seq := GetLastAppliedSeq(); seq != 8 {
		t.Fatalf("direct write moved the log position: %d", seq)
	}
}
