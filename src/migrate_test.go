package main

import (
	"errors"
	"os"
	"testing"
	"time"

	lungo "github.com/256dpi/lungo"

	"github.com/azukaar/cosmos-server/src/utils"
)

func setupMigrateEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir() + "/"
	prev := utils.CONFIGFOLDER
	utils.CONFIGFOLDER = tmp
	t.Cleanup(func() {
		utils.CloseStore()
		utils.CONFIGFOLDER = prev
	})
	return tmp
}

func writeLungoFixture(t *testing.T, path string, users []utils.User, devices []utils.ConstellationDevice) {
	t.Helper()
	opts := lungo.Options{Store: lungo.NewFileStore(path, 0700)}
	client, engine, err := lungo.Open(nil, opts)
	if err != nil {
		t.Fatal("lungo open:", err)
	}
	appId := utils.GetRootAppId()
	for _, u := range users {
		if _, err := client.Database("COSMOS").Collection(appId+"_users").InsertOne(nil, u); err != nil {
			t.Fatal("lungo insert user:", err)
		}
	}
	for _, d := range devices {
		if _, err := client.Database("COSMOS").Collection(appId+"_devices").InsertOne(nil, d); err != nil {
			t.Fatal("lungo insert device:", err)
		}
	}
	engine.Close()
}

func TestMigratePre02236LungoImport(t *testing.T) {
	tmp := setupMigrateEnv(t)

	created := time.Date(2023, 2, 3, 4, 5, 6, 0, time.UTC)
	writeLungoFixture(t, tmp+"database",
		[]utils.User{
			{Nickname: "alice", Password: "hash1", Role: utils.ADMIN, CreatedAt: created},
			// zero times stay zero through the import
			{Nickname: "bob", Password: "", RegisterKey: "rk"},
		},
		[]utils.ConstellationDevice{
			{DeviceName: "node1", IP: "192.168.201.2", APIKey: "apikey1", Fingerprint: "fp1", PublicKey: "pk1", Tags: []string{"gpu"}},
			// blocked device sharing name+ip with the live one (partial-index case)
			{DeviceName: "node1", IP: "192.168.201.2", APIKey: "apikey-old", Blocked: true},
		})

	if err := utils.InitStore(); err != nil {
		t.Fatal("InitStore:", err)
	}

	MigratePre02236()

	// legacy file renamed, not deleted
	if _, err := os.Stat(tmp + "database"); !os.IsNotExist(err) {
		t.Fatal("legacy database file should be gone")
	}
	if _, err := os.Stat(tmp + "database.backup"); err != nil {
		t.Fatal("database.backup missing:", err)
	}

	u, err := utils.GetUser("alice")
	if err != nil || u.Password != "hash1" || u.Role != utils.ADMIN || !u.CreatedAt.Equal(created) {
		t.Fatalf("alice not imported correctly: %+v, %v", u, err)
	}
	b, err := utils.GetUser("bob")
	if err != nil || b.RegisterKey != "rk" || !b.CreatedAt.IsZero() || !b.LastLogin.IsZero() {
		t.Fatalf("bob not imported correctly: %+v, %v", b, err)
	}

	devices, err := utils.ListDevices(true)
	if err != nil || len(devices) != 2 {
		t.Fatalf("devices not imported: %v, %v", devices, err)
	}
	active, err := utils.GetDeviceByName("node1", true)
	if err != nil || active.APIKey != "apikey1" || active.Fingerprint != "fp1" || active.PublicKey != "pk1" {
		t.Fatalf("secrets did not carry over: %+v, %v", active, err)
	}
	if len(active.Tags) != 1 || active.Tags[0] != "gpu" {
		t.Fatalf("tags did not carry over: %+v", active)
	}

	// second run is a no-op (file renamed away)
	MigratePre02236()
	count, _ := utils.CountUsers()
	if count != 2 {
		t.Fatalf("second run must be a no-op, users = %d", count)
	}
	devices, _ = utils.ListDevices(true)
	if len(devices) != 2 {
		t.Fatalf("second run must be a no-op, devices = %d", len(devices))
	}
}

func TestMigratePre02236NoFile(t *testing.T) {
	setupMigrateEnv(t)
	if err := utils.InitStore(); err != nil {
		t.Fatal("InitStore:", err)
	}
	// no legacy file: must be a silent no-op
	MigratePre02236()
	if _, err := utils.GetUser("anyone"); !errors.Is(err, utils.ErrNotFound) {
		t.Fatal("store should stay empty")
	}
}

func TestMigratePre014Gate(t *testing.T) {
	tmp := setupMigrateEnv(t)
	if err := utils.InitStore(); err != nil {
		t.Fatal("InitStore:", err)
	}

	cfg := utils.Config{}

	// no MongoDB configured -> never
	cfg.MongoDB = ""
	if migratePre014Needed(cfg) {
		t.Fatal("gate must be closed without MongoDB")
	}

	// MongoDB configured, empty store, no legacy file -> run
	cfg.MongoDB = "mongodb://example:27017"
	if !migratePre014Needed(cfg) {
		t.Fatal("gate must open for Mongo-only installs")
	}

	// legacy lungo file present -> skip (MigratePre02236 owns it)
	if err := os.WriteFile(tmp+"database", []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if migratePre014Needed(cfg) {
		t.Fatal("gate must be closed when the lungo file exists")
	}
	os.Remove(tmp + "database")

	// store already has data -> skip
	if err := utils.CreateUser(utils.User{Nickname: "existing"}); err != nil {
		t.Fatal(err)
	}
	if migratePre014Needed(cfg) {
		t.Fatal("gate must be closed once auth.db has data")
	}
}
