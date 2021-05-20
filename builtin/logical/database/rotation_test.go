package database

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Sectorbob/mlab-ns2/gae/ns/digest"
	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/helper/testhelpers/mongodb"
	postgreshelper "github.com/hashicorp/vault/helper/testhelpers/postgresql"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/dbtxn"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/lib/pq"
	mongodbatlasapi "github.com/mongodb/go-client-mongodb-atlas/mongodbatlas"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	dbUser                = "vaultstatictest"
	dbUserDefaultPassword = "password"

	testMongoDBRole = `{ "db": "admin", "roles": [ { "role": "readWrite" } ] }`
)

func TestBackend_StaticRole_Rotate_basic(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(context.Background())

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// create the database user
	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)

	verifyPgConn(t, dbUser, dbUserDefaultPassword, connURL)

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	data = map[string]interface{}{
		"name":                "plugin-role-test",
		"db_name":             "plugin-test",
		"rotation_statements": testRoleStaticUpdate,
		"username":            dbUser,
		"rotation_period":     "5400s",
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "static-roles/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}

	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Read the creds
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "static-creds/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}

	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	username := resp.Data["username"].(string)
	password := resp.Data["password"].(string)
	if username == "" || password == "" {
		t.Fatalf("empty username (%s) or password (%s)", username, password)
	}

	// Verify username/password
	verifyPgConn(t, dbUser, password, connURL)

	// Re-read the creds, verifying they aren't changing on read
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "static-creds/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	if username != resp.Data["username"].(string) || password != resp.Data["password"].(string) {
		t.Fatal("expected re-read username/password to match, but didn't")
	}

	// Trigger rotation
	data = map[string]interface{}{"name": "plugin-role-test"}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "rotate-role/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	if resp != nil {
		t.Fatalf("Expected empty response from rotate-role: (%#v)", resp)
	}

	// Re-Read the creds
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "static-creds/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	newPassword := resp.Data["password"].(string)
	if password == newPassword {
		t.Fatalf("expected passwords to differ, got (%s)", newPassword)
	}

	// Verify new username/password
	verifyPgConn(t, username, newPassword, connURL)
}

// Sanity check to make sure we don't allow an attempt of rotating credentials
// for non-static accounts, which doesn't make sense anyway, but doesn't hurt to
// verify we return an error
func TestBackend_StaticRole_Rotate_NonStaticError(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(context.Background())

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// create the database user
	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	data = map[string]interface{}{
		"name":                  "plugin-role-test",
		"db_name":               "plugin-test",
		"creation_statements":   testRoleStaticCreate,
		"rotation_statements":   testRoleStaticUpdate,
		"revocation_statements": defaultRevocationSQL,
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "roles/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}

	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Read the creds
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "creds/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	username := resp.Data["username"].(string)
	password := resp.Data["password"].(string)
	if username == "" || password == "" {
		t.Fatalf("empty username (%s) or password (%s)", username, password)
	}

	// Verify username/password
	verifyPgConn(t, dbUser, dbUserDefaultPassword, connURL)
	// Trigger rotation
	data = map[string]interface{}{"name": "plugin-role-test"}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "rotate-role/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	// expect resp to be an error
	resp, _ = b.HandleRequest(namespace.RootContext(nil), req)
	if !resp.IsError() {
		t.Fatalf("expected error rotating non-static role")
	}

	if resp.Error().Error() != "no static role found for role name" {
		t.Fatalf("wrong error message: %s", err)
	}
}

func TestBackend_StaticRole_Revoke_user(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(context.Background())

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// create the database user
	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	testCases := map[string]struct {
		revoke          *bool
		expectVerifyErr bool
	}{
		// Default case: user does not specify, Vault leaves the database user
		// untouched, and the final connection check passes because the user still
		// exists
		"unset": {},
		// Revoke on delete. The final connection check should fail because the user
		// no longer exists
		"revoke": {
			revoke:          newBoolPtr(true),
			expectVerifyErr: true,
		},
		// Revoke false, final connection check should still pass
		"persist": {
			revoke: newBoolPtr(false),
		},
	}
	for k, tc := range testCases {
		t.Run(k, func(t *testing.T) {
			data = map[string]interface{}{
				"name":                "plugin-role-test",
				"db_name":             "plugin-test",
				"rotation_statements": testRoleStaticUpdate,
				"username":            dbUser,
				"rotation_period":     "5400s",
			}
			if tc.revoke != nil {
				data["revoke_user_on_delete"] = *tc.revoke
			}

			req = &logical.Request{
				Operation: logical.CreateOperation,
				Path:      "static-roles/plugin-role-test",
				Storage:   config.StorageView,
				Data:      data,
			}

			resp, err = b.HandleRequest(namespace.RootContext(nil), req)
			if err != nil || (resp != nil && resp.IsError()) {
				t.Fatalf("err:%s resp:%#v\n", err, resp)
			}

			// Read the creds
			data = map[string]interface{}{}
			req = &logical.Request{
				Operation: logical.ReadOperation,
				Path:      "static-creds/plugin-role-test",
				Storage:   config.StorageView,
				Data:      data,
			}

			resp, err = b.HandleRequest(namespace.RootContext(nil), req)
			if err != nil || (resp != nil && resp.IsError()) {
				t.Fatalf("err:%s resp:%#v\n", err, resp)
			}

			username := resp.Data["username"].(string)
			password := resp.Data["password"].(string)
			if username == "" || password == "" {
				t.Fatalf("empty username (%s) or password (%s)", username, password)
			}

			// Verify username/password
			verifyPgConn(t, username, password, connURL)

			// delete the role, expect the default where the user is not destroyed
			// Read the creds
			req = &logical.Request{
				Operation: logical.DeleteOperation,
				Path:      "static-roles/plugin-role-test",
				Storage:   config.StorageView,
			}

			resp, err = b.HandleRequest(namespace.RootContext(nil), req)
			if err != nil || (resp != nil && resp.IsError()) {
				t.Fatalf("err:%s resp:%#v\n", err, resp)
			}

			// Verify new username/password still work
			verifyPgConn(t, username, password, connURL)
		})
	}
}

func createTestPGUser(t *testing.T, connURL string, username, password, query string) {
	t.Helper()
	log.Printf("[TRACE] Creating test user")
	conn, err := pq.ParseURL(connURL)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("postgres", conn)
	defer db.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Start a transaction
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	m := map[string]string{
		"name":     username,
		"password": password,
	}
	if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
		t.Fatal(err)
	}
	// Commit the transaction
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func verifyPgConn(t *testing.T, username, password, connURL string) {
	t.Helper()
	cURL := strings.Replace(connURL, "postgres:secret", username+":"+password, 1)
	db, err := sql.Open("postgres", cURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
}

// WAL testing
//
// First scenario, WAL contains a role name that does not exist.
func TestBackend_Static_QueueWAL_discard_role_not_found(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	ctx := context.Background()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	_, err := framework.PutWAL(ctx, config.StorageView, staticWALKey, &setCredentialsWAL{
		RoleName: "doesnotexist",
	})
	if err != nil {
		t.Fatalf("error with PutWAL: %s", err)
	}

	assertWALCount(t, config.StorageView, 1, staticWALKey)

	b, err := Factory(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup(ctx)

	time.Sleep(5 * time.Second)
	bd := b.(*databaseBackend)
	if bd.credRotationQueue == nil {
		t.Fatal("database backend had no credential rotation queue")
	}

	// Verify empty queue
	if bd.credRotationQueue.Len() != 0 {
		t.Fatalf("expected zero queue items, got: %d", bd.credRotationQueue.Len())
	}

	assertWALCount(t, config.StorageView, 0, staticWALKey)
}

// Second scenario, WAL contains a role name that does exist, but the role's
// LastVaultRotation is greater than the WAL has
func TestBackend_Static_QueueWAL_discard_role_newer_rotation_date(t *testing.T) {
	t.Skip("temporarily disabled due to intermittent failures")

	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	ctx := context.Background()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	roleName := "test-discard-by-date"
	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// create the database user
	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Save Now() to make sure rotation time is after this, as well as the WAL
	// time
	roleTime := time.Now()

	// Create role
	data = map[string]interface{}{
		"name":                roleName,
		"db_name":             "plugin-test",
		"rotation_statements": testRoleStaticUpdate,
		"username":            dbUser,
		// Low value here, to make sure the backend rotates this password at least
		// once before we compare it to the WAL
		"rotation_period": "10s",
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "static-roles/" + roleName,
		Storage:   config.StorageView,
		Data:      data,
	}

	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Allow the first rotation to occur, setting LastVaultRotation
	time.Sleep(time.Second * 12)

	// Cleanup the backend, then create a WAL for the role with a
	// LastVaultRotation of 1 hour ago, so that when we recreate the backend the
	// WAL will be read but discarded
	b.Cleanup(ctx)
	b = nil
	time.Sleep(time.Second * 3)

	// Make a fake WAL entry with an older time
	oldRotationTime := roleTime.Add(time.Hour * -1)
	walPassword := "somejunkpassword"
	_, err = framework.PutWAL(ctx, config.StorageView, staticWALKey, &setCredentialsWAL{
		RoleName:          roleName,
		NewPassword:       walPassword,
		LastVaultRotation: oldRotationTime,
		Username:          dbUser,
	})
	if err != nil {
		t.Fatalf("error with PutWAL: %s", err)
	}

	assertWALCount(t, config.StorageView, 1, staticWALKey)

	// Reload backend
	lb, err = Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok = lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(ctx)

	// Allow enough time for populateQueue to work after boot
	time.Sleep(time.Second * 12)

	// PopulateQueue should have processed the entry
	assertWALCount(t, config.StorageView, 0, staticWALKey)

	// Read the role
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "static-roles/" + roleName,
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	lastVaultRotation := resp.Data["last_vault_rotation"].(time.Time)
	if !lastVaultRotation.After(oldRotationTime) {
		t.Fatal("last vault rotation time not greater than WAL time")
	}

	if !lastVaultRotation.After(roleTime) {
		t.Fatal("last vault rotation time not greater than role creation time")
	}

	// Grab password to verify it didn't change
	req = &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "static-creds/" + roleName,
		Storage:   config.StorageView,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	password := resp.Data["password"].(string)
	if password == walPassword {
		t.Fatalf("expected password to not be changed by WAL, but was")
	}
}

// Helper to assert the number of WAL entries is what we expect
func assertWALCount(t *testing.T, s logical.Storage, expected int, key string) {
	t.Helper()

	var count int
	ctx := context.Background()
	keys, err := framework.ListWAL(ctx, s)
	if err != nil {
		t.Fatal("error listing WALs")
	}

	// Loop through WAL keys and process any rotation ones
	for _, k := range keys {
		walEntry, _ := framework.GetWAL(ctx, s, k)
		if walEntry == nil {
			continue
		}

		if walEntry.Kind != key {
			continue
		}
		count++
	}
	if expected != count {
		t.Fatalf("WAL count mismatch, expected (%d), got (%d)", expected, count)
	}
}

//
// End WAL testing
//

type userCreator func(t *testing.T, username, password string)

func TestBackend_StaticRole_Rotations_PostgreSQL(t *testing.T) {
	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "latest")
	defer cleanup()
	uc := userCreator(func(t *testing.T, username, password string) {
		createTestPGUser(t, connURL, username, password, testRoleStaticCreate)
	})
	testBackend_StaticRole_Rotations(t, uc, map[string]interface{}{
		"connection_url": connURL,
		"plugin_name":    "postgresql-database-plugin",
	})
}

func TestBackend_StaticRole_Rotations_MongoDB(t *testing.T) {
	cleanup, connURL := mongodb.PrepareTestContainerWithDatabase(t, "latest", "vaulttestdb")
	defer cleanup()

	uc := userCreator(func(t *testing.T, username, password string) {
		testCreateDBUser(t, connURL, "vaulttestdb", username, password)
	})
	testBackend_StaticRole_Rotations(t, uc, map[string]interface{}{
		"connection_url": connURL,
		"plugin_name":    "mongodb-database-plugin",
	})
}

func TestBackend_StaticRole_Rotations_MongoDBAtlas(t *testing.T) {
	// To get the project ID, connect to cloud.mongodb.com, go to the vault-test project and
	// look at Project Settings.
	projID := os.Getenv("VAULT_MONGODBATLAS_PROJECT_ID")
	// For the private and public key, go to Organization Access Manager on cloud.mongodb.com,
	// choose Create API Key, then create one using the defaults.  Then go back to the vault-test
	// project and add the API key to it, with permissions "Project Owner".
	privKey := os.Getenv("VAULT_MONGODBATLAS_PRIVATE_KEY")
	pubKey := os.Getenv("VAULT_MONGODBATLAS_PUBLIC_KEY")
	if projID == "" {
		t.Logf("Skipping MongoDB Atlas test because VAULT_MONGODBATLAS_PROJECT_ID not set")
		t.SkipNow()
	}

	transport := digest.NewTransport(pubKey, privKey)
	cl, err := transport.Client()
	if err != nil {
		t.Fatal(err)
	}

	api, err := mongodbatlasapi.New(cl)
	if err != nil {
		t.Fatal(err)
	}

	uc := userCreator(func(t *testing.T, username, password string) {
		// Delete the user in case it's still there from an earlier run, ignore
		// errors in case it's not.
		_, _ = api.DatabaseUsers.Delete(context.Background(), projID, username)

		req := &mongodbatlasapi.DatabaseUser{
			Username:     username,
			Password:     password,
			DatabaseName: "admin",
			Roles:        []mongodbatlasapi.Role{{RoleName: "atlasAdmin", DatabaseName: "admin"}},
		}
		_, _, err := api.DatabaseUsers.Create(context.Background(), projID, req)
		if err != nil {
			t.Fatal(err)
		}
	})
	testBackend_StaticRole_Rotations(t, uc, map[string]interface{}{
		"plugin_name": "mongodbatlas-database-plugin",
		"project_id":  projID,
		"private_key": privKey,
		"public_key":  pubKey,
	})
}

func testBackend_StaticRole_Rotations(t *testing.T, createUser userCreator, opts map[string]interface{}) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys
	// Change background task interval to 1s to give more margin
	// for it to successfully run during the sleeps below.
	config.Config[queueTickIntervalKey] = "1"

	// Rotation ticker starts running in Factory call
	b, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup(context.Background())

	// allow initQueue to finish
	bd := b.(*databaseBackend)
	if bd.credRotationQueue == nil {
		t.Fatal("database backend had no credential rotation queue")
	}

	// Configure a connection
	data := map[string]interface{}{
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
	}
	for k, v := range opts {
		data[k] = v
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	testCases := []string{"10", "20", "100"}
	// Create database users ahead
	for _, tc := range testCases {
		createUser(t, "statictest"+tc, "test")
	}

	// create three static roles with different rotation periods
	for _, tc := range testCases {
		roleName := "plugin-static-role-" + tc
		data = map[string]interface{}{
			"name":            roleName,
			"db_name":         "plugin-test",
			"username":        "statictest" + tc,
			"rotation_period": tc,
		}

		req = &logical.Request{
			Operation: logical.CreateOperation,
			Path:      "static-roles/" + roleName,
			Storage:   config.StorageView,
			Data:      data,
		}

		resp, err = b.HandleRequest(namespace.RootContext(nil), req)
		if err != nil || (resp != nil && resp.IsError()) {
			t.Fatalf("err:%s resp:%#v\n", err, resp)
		}
	}

	// verify the queue has 3 items in it
	if bd.credRotationQueue.Len() != 3 {
		t.Fatalf("expected 3 items in the rotation queue, got: (%d)", bd.credRotationQueue.Len())
	}

	// List the roles
	data = map[string]interface{}{}
	req = &logical.Request{
		Operation: logical.ListOperation,
		Path:      "static-roles/",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	keys := resp.Data["keys"].([]string)
	if len(keys) != 3 {
		t.Fatalf("expected 3 roles, got: (%d)", len(keys))
	}

	// capture initial passwords, before the periodic function is triggered
	pws := make(map[string][]string, 0)
	pws = capturePasswords(t, b, config, testCases, pws)

	// sleep to make sure the periodic func has time to actually run
	time.Sleep(15 * time.Second)
	pws = capturePasswords(t, b, config, testCases, pws)

	// sleep more, this should allow both sr10 and sr20 to rotate
	time.Sleep(10 * time.Second)
	pws = capturePasswords(t, b, config, testCases, pws)

	// verify all pws are as they should
	pass := true
	for k, v := range pws {
		if len(v) < 3 {
			t.Fatalf("expected to find 3 passwords for (%s), only found (%d)", k, len(v))
		}
		switch {
		case k == "plugin-static-role-10":
			// expect all passwords to be different
			if v[0] == v[1] || v[1] == v[2] || v[0] == v[2] {
				pass = false
			}
		case k == "plugin-static-role-20":
			// expect the first two to be equal, but different from the third
			if v[0] != v[1] || v[0] == v[2] {
				pass = false
			}
		case k == "plugin-static-role-100":
			// expect all passwords to be equal
			if v[0] != v[1] || v[1] != v[2] {
				pass = false
			}
		default:
			t.Fatalf("unexpected password key: %v", k)
		}
	}
	if !pass {
		t.Fatalf("password rotations did not match expected: %#v", pws)
	}
}

func testCreateDBUser(t testing.TB, connURL, db, username, password string) {
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connURL))
	if err != nil {
		t.Fatal(err)
	}

	createUserCmd := &createUserCommand{
		Username: username,
		Password: password,
		Roles:    []interface{}{},
	}
	result := client.Database(db).RunCommand(ctx, createUserCmd, nil)
	if result.Err() != nil {
		t.Fatal(result.Err())
	}
}

type createUserCommand struct {
	Username string        `bson:"createUser"`
	Password string        `bson:"pwd"`
	Roles    []interface{} `bson:"roles"`
}

// Demonstrates a bug fix for the credential rotation not releasing locks
func TestBackend_StaticRole_LockRegression(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(context.Background())

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)
	for i := 0; i < 25; i++ {
		data := map[string]interface{}{
			"name":                "plugin-role-test",
			"db_name":             "plugin-test",
			"rotation_statements": testRoleStaticUpdate,
			"username":            dbUser,
			"rotation_period":     "7s",
		}

		req = &logical.Request{
			Operation: logical.UpdateOperation,
			Path:      "static-roles/plugin-role-test",
			Storage:   config.StorageView,
			Data:      data,
		}

		resp, err = b.HandleRequest(namespace.RootContext(nil), req)
		if err != nil || (resp != nil && resp.IsError()) {
			t.Fatalf("err:%s resp:%#v\n", err, resp)
		}

		// sleeping is needed to trigger the deadlock, otherwise things are
		// processed too quickly to trigger the rotation lock on so few roles
		time.Sleep(500 * time.Millisecond)
	}
}

func TestBackend_StaticRole_Rotate_Invalid_Role(t *testing.T) {
	cluster, sys := getCluster(t)
	defer cluster.Cleanup()

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	config.System = sys

	lb, err := Factory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := lb.(*databaseBackend)
	if !ok {
		t.Fatal("could not convert to db backend")
	}
	defer b.Cleanup(context.Background())

	cleanup, connURL := postgreshelper.PrepareTestContainer(t, "")
	defer cleanup()

	// create the database user
	createTestPGUser(t, connURL, dbUser, dbUserDefaultPassword, testRoleStaticCreate)

	verifyPgConn(t, dbUser, dbUserDefaultPassword, connURL)

	// Configure a connection
	data := map[string]interface{}{
		"connection_url":    connURL,
		"plugin_name":       "postgresql-database-plugin",
		"verify_connection": false,
		"allowed_roles":     []string{"*"},
		"name":              "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/plugin-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err := b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	data = map[string]interface{}{
		"name":                "plugin-role-test",
		"db_name":             "plugin-test",
		"rotation_statements": testRoleStaticUpdate,
		"username":            dbUser,
		"rotation_period":     "5400s",
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "static-roles/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}

	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Pop manually key to emulate a queue without existing key
	b.credRotationQueue.PopByKey("plugin-role-test")

	// Make sure queue is empty
	if b.credRotationQueue.Len() != 0 {
		t.Fatalf("expected queue length to be 0 but is %d", b.credRotationQueue.Len())
	}

	// Trigger rotation
	data = map[string]interface{}{"name": "plugin-role-test"}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "rotate-role/plugin-role-test",
		Storage:   config.StorageView,
		Data:      data,
	}
	resp, err = b.HandleRequest(namespace.RootContext(nil), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// Check if key is in queue
	if b.credRotationQueue.Len() != 1 {
		t.Fatalf("expected queue length to be 1 but is %d", b.credRotationQueue.Len())
	}
}

// capturePasswords captures the current passwords at the time of calling, and
// returns a map of username / passwords building off of the input map
func capturePasswords(t *testing.T, b logical.Backend, config *logical.BackendConfig, testCases []string, pws map[string][]string) map[string][]string {
	new := make(map[string][]string, 0)
	for _, tc := range testCases {
		// Read the role
		roleName := "plugin-static-role-" + tc
		req := &logical.Request{
			Operation: logical.ReadOperation,
			Path:      "static-creds/" + roleName,
			Storage:   config.StorageView,
		}
		resp, err := b.HandleRequest(namespace.RootContext(nil), req)
		if err != nil || (resp != nil && resp.IsError()) {
			t.Fatalf("err:%s resp:%#v\n", err, resp)
		}

		username := resp.Data["username"].(string)
		password := resp.Data["password"].(string)
		if username == "" || password == "" {
			t.Fatalf("expected both username/password for (%s), got (%s), (%s)", roleName, username, password)
		}
		new[roleName] = append(new[roleName], password)
	}

	for k, v := range new {
		pws[k] = append(pws[k], v...)
	}

	return pws
}

func newBoolPtr(b bool) *bool {
	v := b
	return &v
}