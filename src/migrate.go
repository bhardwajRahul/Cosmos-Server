package main 

import (
	"os"
	"strconv"

	lungo "github.com/256dpi/lungo"

	"github.com/azukaar/cosmos-server/src/utils"
)

// DNS providers renamed in lego v5, credentials are unchanged.
// acme-dns/rfc2136/webnames still resolve as aliases, but are no longer listed in dns-list.json
var legoV5RenamedProviders = map[string]string{
	"azure":    "azuredns",
	"acme-dns": "acmedns",
	"rfc2136":  "dnsupdate",
	"webnames": "webnamesru",
}

// DNS providers renamed in lego v5, but backed by a different API so credentials must be re-entered
var legoV5RenamedProvidersNewCreds = map[string]string{
	"dnspod": "tencentcloud",
	"iij":    "iijdpf",
}

// DNS providers dropped in lego v5 with no replacement
var legoV5RemovedProviders = []string{
	"brandit",
	"cloudxns",
	"googledomains",
	"iwantmyname",
}

func MigratePre02231() {
	config := utils.ReadConfigFromFile()
	provider := config.HTTPConfig.DNSChallengeProvider

	if provider == "" {
		return
	}

	if newName, ok := legoV5RenamedProviders[provider]; ok {
		utils.Log("MigratePre02231: renaming DNS challenge provider " + provider + " to " + newName)
		config.HTTPConfig.DNSChallengeProvider = newName
		utils.SetBaseMainConfig(config)
		return
	}

	if newName, ok := legoV5RenamedProvidersNewCreds[provider]; ok {
		utils.Log("MigratePre02231: renaming DNS challenge provider " + provider + " to " + newName)
		config.HTTPConfig.DNSChallengeProvider = newName
		utils.SetBaseMainConfig(config)
		utils.MajorError("MigratePre02231: DNS provider '" + provider + "' is now '" + newName + "' and uses a different API. Update its credentials in the HTTPS settings or certificate renewal will fail", nil)
		return
	}

	for _, removed := range legoV5RemovedProviders {
		if provider == removed {
			utils.MajorError("MigratePre02231: DNS provider '" + provider + "' no longer exists and has no replacement. Pick another provider in the HTTPS settings or certificate renewal will fail", nil)
			return
		}
	}
}

// MigratePre02236 one-shot imports the legacy lungo embedded database into auth.db.
// The rename to database.backup makes it naturally one-shot. Requires InitStore().
func MigratePre02236() {
	dbPath := utils.CONFIGFOLDER + "database"
	if _, err := os.Stat(dbPath); err != nil {
		return
	}

	utils.Log("MigratePre02236: importing legacy embedded database into auth.db...")

	opts := lungo.Options{
		Store: lungo.NewFileStore(dbPath, 0700),
	}

	client, engine, err := lungo.Open(nil, opts)
	if err != nil {
		utils.MajorError("MigratePre02236: cannot open legacy embedded database", err)
		return
	}

	name := os.Getenv("MONGODB_NAME")
	if name == "" {
		name = "COSMOS"
	}
	applicationId := utils.GetRootAppId()

	users := []utils.User{}
	devices := []utils.ConstellationDevice{}

	curU, err := client.Database(name).Collection(applicationId + "_users").Find(nil, map[string]interface{}{})
	if err == nil {
		err = curU.All(nil, &users)
	}
	if err != nil {
		utils.MajorError("MigratePre02236: cannot read legacy users", err)
		engine.Close()
		return
	}

	curD, err := client.Database(name).Collection(applicationId + "_devices").Find(nil, map[string]interface{}{})
	if err == nil {
		err = curD.All(nil, &devices)
	}
	if err != nil {
		utils.MajorError("MigratePre02236: cannot read legacy devices", err)
		engine.Close()
		return
	}

	engine.Close()

	// one tx via the store layer
	ms := []utils.Mutation{}
	if len(users) > 0 {
		ms = append(ms, utils.Mutation{Table: "users", Op: "insertMany", Doc: users})
	}
	if len(devices) > 0 {
		ms = append(ms, utils.Mutation{Table: "devices", Op: "insertMany", Doc: devices})
	}
	if len(ms) > 0 {
		if err := utils.CommitMutations(ms); err != nil {
			utils.MajorError("MigratePre02236: cannot import legacy data into auth.db", err)
			return
		}
	}

	if err := os.Rename(dbPath, dbPath+".backup"); err != nil {
		utils.MajorError("MigratePre02236: cannot rename legacy database file", err)
		return
	}

	utils.Log("MigratePre02236: imported " + strconv.Itoa(len(users)) + " users and " + strconv.Itoa(len(devices)) + " devices; legacy file renamed to database.backup")
}
