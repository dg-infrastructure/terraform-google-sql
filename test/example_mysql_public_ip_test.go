package test

import (
	"database/sql"
	"fmt"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/dialers/mysql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gruntwork-io/terratest/modules/gcp"
	"github.com/gruntwork-io/terratest/modules/logger"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const NAME_PREFIX_PUBLIC = "mysql-public"
const EXAMPLE_NAME_PUBLIC = "mysql-public-ip"

func TestMySqlPublicIP(t *testing.T) {
	t.Parallel()

	//os.Setenv("SKIP_bootstrap", "true")
	//os.Setenv("SKIP_deploy", "true")
	//os.Setenv("SKIP_validate_outputs", "true")
	//os.Setenv("SKIP_sql_tests", "true")
	//os.Setenv("SKIP_proxy_tests", "true")
	//os.Setenv("SKIP_teardown", "true")

	_examplesDir := test_structure.CopyTerraformFolderToTemp(t, "../", "examples")
	exampleDir := filepath.Join(_examplesDir, EXAMPLE_NAME_PUBLIC)

	// BOOTSTRAP VARIABLES FOR THE TESTS
	test_structure.RunTestStage(t, "bootstrap", func() {
		projectId := gcp.GetGoogleProjectIDFromEnvVar(t)
		region := getRandomRegion(t, projectId)

		test_structure.SaveString(t, exampleDir, KEY_REGION, region)
		test_structure.SaveString(t, exampleDir, KEY_PROJECT, projectId)
	})

	// AT THE END OF THE TESTS, RUN `terraform destroy`
	// TO CLEAN UP ANY RESOURCES THAT WERE CREATED
	defer test_structure.RunTestStage(t, "teardown", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, exampleDir)
		terraform.Destroy(t, terraformOptions)
	})

	test_structure.RunTestStage(t, "deploy", func() {
		region := test_structure.LoadString(t, exampleDir, KEY_REGION)
		projectId := test_structure.LoadString(t, exampleDir, KEY_PROJECT)
		terraformOptions := createTerratestOptionsForMySql(projectId, region, exampleDir, NAME_PREFIX_PUBLIC)
		test_structure.SaveTerraformOptions(t, exampleDir, terraformOptions)

		terraform.InitAndApply(t, terraformOptions)
	})

	// VALIDATE MODULE OUTPUTS
	test_structure.RunTestStage(t, "validate_outputs", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, exampleDir)

		region := test_structure.LoadString(t, exampleDir, KEY_REGION)
		projectId := test_structure.LoadString(t, exampleDir, KEY_PROJECT)

		instanceNameFromOutput := terraform.Output(t, terraformOptions, OUTPUT_INSTANCE_NAME)
		dbNameFromOutput := terraform.Output(t, terraformOptions, OUTPUT_DB_NAME)
		proxyConnectionFromOutput := terraform.Output(t, terraformOptions, OUTPUT_PROXY_CONNECTION)

		expectedDBConn := fmt.Sprintf("%s:%s:%s", projectId, region, instanceNameFromOutput)

		assert.True(t, strings.HasPrefix(instanceNameFromOutput, NAME_PREFIX_PUBLIC))
		assert.Equal(t, DB_NAME, dbNameFromOutput)
		assert.Equal(t, expectedDBConn, proxyConnectionFromOutput)
	})

	// TEST REGULAR SQL CLIENT
	test_structure.RunTestStage(t, "sql_tests", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, exampleDir)

		publicIp := terraform.Output(t, terraformOptions, OUTPUT_PUBLIC_IP)

		connectionString := fmt.Sprintf("%s:%s@tcp(%s:3306)/%s", DB_USER, DB_PASS, publicIp, DB_NAME)

		// Does not actually open up the connection - just returns a DB ref
		logger.Logf(t, "Connecting to: %s", publicIp)
		db, err := sql.Open("mysql",
			connectionString)
		require.NoError(t, err, "Failed to open DB connection")

		// Make sure we clean up properly
		defer db.Close()

		// Run ping to actually test the connection
		logger.Log(t, "Ping the DB")
		if err = db.Ping(); err != nil {
			t.Fatalf("Failed to ping DB: %v", err)
		}

		// Create table if not exists
		logger.Logf(t, "Create table: %s", MYSQL_CREATE_TEST_TABLE_WITH_AUTO_INCREMENT_STATEMENT)
		if _, err = db.Exec(MYSQL_CREATE_TEST_TABLE_WITH_AUTO_INCREMENT_STATEMENT); err != nil {
			t.Fatalf("Failed to create table: %v", err)
		}

		// Clean up
		logger.Logf(t, "Empty table: %s", MYSQL_EMPTY_TEST_TABLE_STATEMENT)
		if _, err = db.Exec(MYSQL_EMPTY_TEST_TABLE_STATEMENT); err != nil {
			t.Fatalf("Failed to clean up table: %v", err)
		}

		// Insert data to check that our auto-increment flags worked
		logger.Logf(t, "Insert data: %s", MYSQL_INSERT_TEST_ROW)
		stmt, err := db.Prepare(MYSQL_INSERT_TEST_ROW)
		require.NoError(t, err, "Failed to prepare statement")

		// Execute the statement
		res, err := stmt.Exec("Grunt")
		require.NoError(t, err, "Failed to execute statement")

		// Get the last insert id
		lastId, err := res.LastInsertId()
		require.NoError(t, err, "Failed to get last insert id")

		// Since we set the auto increment to 5, modulus should always be 0
		assert.Equal(t, int64(0), int64(lastId%5))
	})

	// TEST CLOUD SQL PROXY
	test_structure.RunTestStage(t, "proxy_tests", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, exampleDir)

		proxyConn := terraform.Output(t, terraformOptions, OUTPUT_PROXY_CONNECTION)

		logger.Logf(t, "Connecting to: %s via Cloud SQL Proxy", proxyConn)

		// Use the Cloud SQL Proxy for queries
		// See https://cloud.google.com/sql/docs/mysql/sql-proxy
		cfg := mysql.Cfg(proxyConn, DB_USER, DB_PASS)
		cfg.DBName = DB_NAME
		cfg.ParseTime = true

		const timeout = 10 * time.Second
		cfg.Timeout = timeout
		cfg.ReadTimeout = timeout
		cfg.WriteTimeout = timeout

		// Dial in. This one actually pings the database already
		db, err := mysql.DialCfg(cfg)
		require.NoError(t, err, "Failed to open Proxy DB connection")

		// Make sure we clean up properly
		defer db.Close()

		// Run ping to actually test the connection
		logger.Log(t, "Ping the DB")
		if err = db.Ping(); err != nil {
			t.Fatalf("Failed to ping DB via Proxy: %v", err)
		}

		// Insert data to check that our auto-increment flags worked
		logger.Logf(t, "Insert data: %s", MYSQL_INSERT_TEST_ROW)
		stmt, err := db.Prepare(MYSQL_INSERT_TEST_ROW)
		require.NoError(t, err, "Failed to prepare proxy statement")

		// Execute the statement
		res, err := stmt.Exec("Grunt2")
		require.NoError(t, err, "Failed to execute proxy statement")

		// Get the last insert id
		lastId, err := res.LastInsertId()
		require.NoError(t, err, "Failed to get last proxy insert id")

		// Since we set the auto increment to 5, modulus should always be 0
		assert.Equal(t, int64(0), int64(lastId%5))
	})
}
