package main 

import (
	"os"
	"strconv"
	"strings"
	"context"
	// "fmt"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/bson"

	lungo "github.com/256dpi/lungo"

	"github.com/azukaar/cosmos-server/src/utils"
	"github.com/azukaar/cosmos-server/src/docker"
)

func MigratePre014Coll(collection string, from *mongo.Client) {
	name := os.Getenv("MONGODB_NAME")
	if name == "" {
			name = "COSMOS"
	}

	utils.Log("Migrating collection " + collection + " from database " + name)

	applicationId := utils.GetRootAppId()

	cf := from.Database(name).Collection(applicationId + "_" + collection)

	cur, err := cf.Find(context.Background(), bson.D{}, options.Find())
	if err != nil {
			utils.Error("Error getting documents from " + collection + " collection", err)
			return
	}
	defer cur.Close(context.Background())

	// typed decode + store-layer write
	switch collection {
	case "users":
		users := []utils.User{}
		if err := cur.All(context.Background(), &users); err != nil {
			utils.Error("Error decoding documents from " + collection + " collection", err)
			return
		}
		if len(users) == 0 {
			return
		}
		if err := utils.CommitMutation(utils.Mutation{Table: "users", Op: "insertMany", Doc: users}); err != nil {
			utils.Error("Error inserting documents into " + collection + " collection", err)
		}
	case "devices":
		devices := []utils.ConstellationDevice{}
		if err := cur.All(context.Background(), &devices); err != nil {
			utils.Error("Error decoding documents from " + collection + " collection", err)
			return
		}
		if len(devices) == 0 {
			return
		}
		if err := utils.CommitMutation(utils.Mutation{Table: "devices", Op: "insertMany", Doc: devices}); err != nil {
			utils.Error("Error inserting documents into " + collection + " collection", err)
		}
	default:
		utils.Error("MigratePre014Coll: unknown collection " + collection, nil)
	}
}

// migratePre014Needed: only if no legacy lungo file and the store is still empty
// (auth.db is created by InitStore before migrations, so "auth.db does not exist"
// is expressed as "store has no data") and MongoDB is configured.
func migratePre014Needed(config utils.Config) bool {
	if config.MongoDB == "" {
		return false
	}
	if _, err := os.Stat(utils.CONFIGFOLDER + "database"); err == nil {
		return false
	}
	// a read failure must skip the migration, never trigger a re-import
	users, errU := utils.CountUsers()
	devices, errD := utils.CountDevices(map[string]interface{}{})
	if errU != nil || errD != nil {
		utils.Error("MigratePre014: cannot inspect auth.db, skipping migration gate", errU)
		return false
	}
	return users == 0 && devices == 0
}


func MigratePre014() {
	config := utils.GetMainConfig()

	if migratePre014Needed(config) {
		utils.Log("MigratePre014: Migration of database...")

		// connect to MongoDB
		utils.Log("Connecting to MongoDB...")
		
		mongoURL := utils.GetMainConfig().MongoDB

		var err error

		opts := options.Client().ApplyURI(mongoURL).SetRetryWrites(true).SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
		
		if !utils.IsInsideContainer || utils.IsHostNetwork {
			hostname := opts.Hosts[0]
			// split port
			hostnameParts := strings.Split(hostname, ":")
			hostname = hostnameParts[0]
			port := "27017" 

			if len(hostnameParts) > 1 {
				port = hostnameParts[1]
			}

			utils.Log("Getting Mongo DB IP from name : " + hostname + " (port " + port + ")")

			ip, _ := utils.GetContainerIPByName(hostname)
			if ip != "" {
				// IsDBaContainer = true
				opts.SetHosts([]string{ip + ":" + port})
				utils.Log("Mongo DB IP : " + ip)
			}
		}
	
		client, err := mongo.Connect(context.TODO(), opts)

		if err != nil {
			panic(err)
		}

		// Ping the primary
		if err := client.Ping(context.TODO(), readpref.Primary()); err != nil {
			panic(err)
		}

		utils.Log("Successfully connected to the database.")

		MigratePre014Coll("users", client)
		MigratePre014Coll("devices", client)

		

		// Migrate DB to puppet mode

		utils.DB()

		if utils.DBContainerName != "" {
			utils.Log("Migrating database to puppet mode...")

			mongoContainer, err := docker.InspectContainer(utils.DBContainerName)
			if err != nil {
				utils.Fatal("MigratePre014 - Cannot migrate database to puppet mode, container " + utils.DBContainerName + " not found", err)
				return
			}

			dbVolume := ""
			dbConfigVolume := ""

			for _, mount := range mongoContainer.Mounts {
				if mount.Destination == "/data/db" {
					dbVolume = mount.Name
				} else if mount.Destination == "/data/configdb" {
					dbConfigVolume = mount.Name
				}
			}

			if dbVolume == "" || dbConfigVolume == "" {
				utils.Error("MigratePre014 - Cannot migrate database to puppet mode, volumes not found", nil)
				MigratePre014_FallBackNoPuppet()
				return
			}

			currentVersion := docker.GetEnv(mongoContainer.Config.Env, "MONGO_VERSION")
			username := docker.GetEnv(mongoContainer.Config.Env, "ME_CONFIG_MONGODB_ADMINUSERNAME")
			if username == "" {
				username = docker.GetEnv(mongoContainer.Config.Env, "MONGO_INITDB_ROOT_USERNAME")
			}
			password := docker.GetEnv(mongoContainer.Config.Env, "ME_CONFIG_MONGODB_ADMINPASSWORD")
			if password == "" {
				password = docker.GetEnv(mongoContainer.Config.Env, "MONGO_INITDB_ROOT_PASSWORD")
			}

			if currentVersion == "" {
				utils.Error("MigratePre014 - Cannot migrate database to puppet mode, version not found", nil)
				MigratePre014_FallBackNoPuppet()
				return
			}

			
			if username == "" || password == "" {
				utils.Error("MigratePre014 - Cannot migrate database to puppet mode, credentials not found", nil)
				MigratePre014_FallBackNoPuppet()
				return
			}

			dbconfig := utils.DatabaseConfig{
				PuppetMode: true,
				Hostname: utils.DBContainerName,
				DbVolume: dbVolume,
				ConfigVolume: dbConfigVolume,
				Version: strings.Split(currentVersion, ".")[0],
				Username: username,
				Password: password,
			}
			
			config.Database = dbconfig

			utils.SetBaseMainConfig(config)
		}
	}
}

func MigratePre014_FallBackNoPuppet() {
	config := utils.ReadConfigFromFile()
	config.Database = utils.DatabaseConfig{
		PuppetMode: false,
	}
	utils.SetBaseMainConfig(config)
}

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

	curU, err := client.Database(name).Collection(applicationId + "_users").Find(nil, bson.D{})
	if err == nil {
		err = curU.All(nil, &users)
	}
	if err != nil {
		utils.MajorError("MigratePre02236: cannot read legacy users", err)
		engine.Close()
		return
	}

	curD, err := client.Database(name).Collection(applicationId + "_devices").Find(nil, bson.D{})
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
