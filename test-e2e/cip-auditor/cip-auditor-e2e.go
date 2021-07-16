/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	guuid "github.com/google/uuid"
	yaml "gopkg.in/yaml.v2"

	"k8s.io/klog/v2"
	"sigs.k8s.io/k8s-container-image-promoter/internal/version"
	"sigs.k8s.io/k8s-container-image-promoter/legacy/audit"
	reg "sigs.k8s.io/k8s-container-image-promoter/legacy/dockerregistry"
	"sigs.k8s.io/k8s-container-image-promoter/legacy/gcloud"
	"sigs.k8s.io/k8s-container-image-promoter/legacy/stream"
	"sigs.k8s.io/release-utils/command"
)

// nolint[lll]
func main() {
	// NOTE: We can't run the tests in parallel because we only have 1 pair of
	// staging/prod GCRs.

	testsPtr := flag.String(
		"tests", "", "the e2e tests file (YAML) to load (REQUIRED)")
	repoRootPtr := flag.String(
		"repo-root", "", "the absolute path of the CIP git repository on disk")
	keyFilePtr := flag.String(
		"key-file", "", "the .json key file to use to activate the service account in the tests (tests only support using 1 service account)")
	helpPtr := flag.Bool(
		"help",
		false,
		"print help")

	flag.Parse()

	// Log linker flags
	printVersion()

	if len(os.Args) == 1 {
		printUsage()
		os.Exit(0)
	}

	if *helpPtr {
		printUsage()
		os.Exit(0)
	}

	if *repoRootPtr == "" {
		klog.Fatal(fmt.Errorf("-repo-root=... flag is required"))
	}

	if len(*keyFilePtr) > 0 {
		if err := gcloud.ActivateServiceAccount(*keyFilePtr); err != nil {
			klog.Fatal("could not activate service account from .json", err)
		}
	}

	runE2ETests(*testsPtr, *repoRootPtr)
}

// nolint[funlen]
func runE2ETests(testsFile, repoRoot string) {

	// Start tests
	ts, err := readE2ETests(testsFile)
	if err != nil {
		klog.Fatal(err)
	}

	// Loop through each e2e test case.
	//
	// For each test, we have to:
	//
	// (1) Clear Cloud Run logs (because this is what we use to check each
	// test).
	//
	// (2) Set up the GCR state. It may be that some tests require the state to
	// be empty (e.g., for checking the INSERT Pub/Sub message for adding
	// images), and that others require the state to be prepopulated with images
	// (e.g., for checking the DELETE Pub/Sub message for deleting images).
	//
	// (3) Set up the Cloud Run state. Namely, clear any existing Cloud Run
	// applications in the test project. Also, clear all Cloud Run logs, because
	// we'll be grepping logs to verify that the auditor handled changes in GCR
	// state correctly.
	//
	// (4) Modify GCR as the test defines.
	//
	// (5) Check Cloud Run logs.

	// Obtain project information defined in workspace_status.sh
	status := getWorkspaceStatus(repoRoot)
	projectID := status["STABLE_TEST_AUDIT_PROJECT_ID"]
	projectNumber := status["STABLE_TEST_AUDIT_PROJECT_NUMBER"]
	invokerServiceAccount := status["STABLE_TEST_AUDIT_INVOKER_SERVICE_ACCOUNT"]
	pushRepo := status["STABLE_TEST_AUDIT_STAGING_IMG_REPOSITORY"]

	// TODO: All of the workspace options, not just this one, should be non-empty
	// values.
	if pushRepo == "" {
		klog.Fatal(
			"could not dereference STABLE_TEST_AUDIT_STAGING_IMG_REPOSITORY")
	}

	// Enable some APIs. These are required in order to run some of the other
	// commands below.
	if err := enableServiceUsageAPI(projectID); err != nil {
		klog.Fatal("error enabling Service Usage API", err)
	}
	if err := enableCloudResourceManagerAPI(projectID); err != nil {
		klog.Fatal("error enabling Cloud Resource Manager API", err)
	}
	if err := enableStackdriverAPI(projectID); err != nil {
		klog.Fatal("error enabling Stackdriver API", err)
	}
	if err := enableStackdriverErrorReportingAPI(projectID); err != nil {
		klog.Fatal("error enabling Stackdriver Error Reporting API", err)
	}
	if err := enableCloudRunAPI(projectID); err != nil {
		klog.Fatal("error enabling Cloud Run API", err)
	}

	// Allow Pub/Sub to create auth tokens for the project.
	if err := enablePubSubTokenCreation(projectNumber, projectID); err != nil {
		klog.Fatal("error giving token creation permissions to Pub/Sub account", err)
	}

	// Clearing the GCR topic is necessary as it will prevent messages regarding
	// anything extraneous of the test from being delivered to the subscription
	// we create below. Some examples of extraneous messages are those Pub/Sub
	// messages relating to the pushing of golden images that are not part of
	// the test case per se.
	if err := clearPubSubTopic(projectID, "gcr"); err != nil {
		klog.Fatal("error resetting Pub/Sub topic 'gcr'", err)
	}

	for _, t := range ts {
		// Generate a UUID for each test case, to make grepping logs easier.
		uuid := guuid.New().String()

		fmt.Printf("\n===> Running e2e test '%s' (%s)...\n", t.Name, uuid)
		err := testSetup(repoRoot, pushRepo, projectNumber, projectID, t)
		if err != nil {
			klog.Fatal("error with test setup stage:", err)
		}

		// Run all setup commands found in the e2e test. Because we cannot allow
		// arbitrary command execution (imagine malicious PRs that change the
		// command execution to some other command), we only allow certain
		// whitelisted commands to be executed.
		if err := runCheckedCommands(t.SetupExtra); err != nil {
			klog.Fatal("error with custom test setup stage:", err)
		}

		// Deploy cloud run instance.
		if err := deployCloudRun(
			repoRoot,
			pushRepo,
			t.ManifestDir,
			projectID,
			uuid,
			invokerServiceAccount); err != nil {
			klog.Fatal("error with deploying Cloud Run service:", err)
		}

		// NOTE: We do not delete the Pub/Sub topic named "gcr" (the topic where
		// GCR posts messages for mutation) because (1) deleting a topic does
		// not drain the subscriptions and (2) according to docs [1] recreating
		// a topic after a deletion may be met with some delay. Because of this,
		// we instead clear all old Pub/Sub messages by deleting and recreating
		// the subscription. This is necessary because the subscription that
		// will act as the link between GCR and Cloud Run must have a push
		// endpoint specified that points to the Cloud Run instance's
		// publicly-accessible HTTPS endpoint.

		// Give the service account permissions to invoke the instance we just
		// deployed.
		if err := empowerServiceAccount(
			projectID, invokerServiceAccount); err != nil {
			klog.Fatal("error with empowering the invoker service account:", err)
		}

		// Create a Pub/Sub subscription with the service account.
		if err := createPubSubSubscription(
			projectID,
			invokerServiceAccount); err != nil {
			klog.Fatal("error with creating the Pub/Sub subscription:", err)
		}

		// Purge all pending Pub/Sub messages up to this point (just before we
		// start mutating state in GCR) because it can make the logs noisy.
		if err := clearPubSubMessages(projectID); err != nil {
			klog.Fatal("error with purging pre-test Pub/Sub messages:", err)
		}

		// Mutate the GCR state (these should all be noticed by the auditor).
		if err := runCheckedCommands(t.Mutations); err != nil {
			klog.Fatal("error with mutations stage:", err)
		}

		// Ensure that the auditor behaved as expected by checking the logs.
		//
		// NOTE: This cannot succeed immediately after the mutations occur,
		// because there is some delay (on the order of ~5 seconds) until the
		// Pub/Sub message from GCR gets processed into an HTTP request to the
		// Cloud Run instance (courtesy of Cloud Run's backend). So we have to
		// allow for some delay. We try 3 times, waiting 6 seconds each time.
		for i := 1; i <= maxLogMatchAttempts; i++ {
			time.Sleep(15 * time.Second)
			if err := checkLogs(projectID, uuid, t.LogMatch); err != nil {
				msg := "error with checking the logs ((%s), attempt #%d of %d): %s"
				if i == maxLogMatchAttempts {
					klog.Fatalf(msg, uuid, i, maxLogMatchAttempts, err)
				}
				klog.Warningf(msg, uuid, i, maxLogMatchAttempts, err)
			} else {
				klog.Infof("checkLogs succeeded for %s", t.LogMatch)
				break
			}
		}

		fmt.Printf("\n===> e2e test '%s' (%s): OK\n", t.Name, uuid)
	}
}

// testSetup clears all repositories listed in the test, then populates the
// staging registry with some images we can use to populate the prod registry.
// We exploit the fact that GCR has 3 regions (us, asia, eu) and just use one of
// the regions to behave as the staging registry.
//
// [1]: https://cloud.google.com/pubsub/docs/admin#pubsub-delete-topic-gcloud
//
// nolint[funlen]
func testSetup(
	repoRoot, pushRepo, projectNumber, projectID string,
	t E2ETest,
) error {

	// Clear GCR state.
	if err := t.clearRepositories(); err != nil {
		return err
	}

	// Clear Cloud Run logs.
	if err := clearLogs(projectID); err != nil {
		return err
	}

	// Clear Error Reporting events.
	if err := clearErrorReporting(projectID); err != nil {
		return err
	}

	// Clear any existing Cloud Run instance.
	if err := clearCloudRun(projectID); err != nil {
		return err
	}

	// Clear any existing subscriptions that are pointing to stale HTTPS push
	// endpoints of old Cloud Run instances (from previous tests). Even though
	// we'll be creating a new subscription with the same name, it's OK
	// according to the documentation.
	if err := clearSubscription(projectID); err != nil {
		return err
	}

	return populateGoldenImages(repoRoot)
}

// nolint[funlen]
func populateGoldenImages(repoRoot string) error {
	goldenPush := fmt.Sprintf("%s/test-e2e/golden-images/push-golden.sh", repoRoot)
	cmd := command.NewWithWorkDir(
		repoRoot,
		goldenPush,
		"--audit",
	)

	klog.Infof("executing %s\n", cmd.String())

	std, err := cmd.RunSuccessOutput()
	fmt.Println(std.Output())
	fmt.Println(std.Error())
	return err
}

// TODO: De-dupe with other e2e functions
func getWorkspaceStatus(repoRoot string) map[string]string {
	fmt.Println("Reading workspace variables")

	status := make(map[string]string)
	cmd := command.NewWithWorkDir(repoRoot, "./workspace_status.sh")
	std, err := cmd.RunSuccessOutput()
	if err != nil {
		klog.Errorln(err)
		return status
	}

	stdout := std.Output()
	for idx, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		words := strings.Split(line, " ")
		if len(words) != 2 {
			klog.Fatalf("ERROR: Unexpected key value pair when parsing workspace_status.sh. Line %d: %q", idx, line)
		}
		status[words[0]] = words[1]
	}
	return status
}

func (t *E2ETest) clearRepositories() error {
	// We need a SyncContext to clear the repos. That's it. The actual
	// promotions will be done by the cip binary, not this tool.
	sc, err := reg.MakeSyncContext(
		[]reg.Manifest{
			{Registries: t.Registries},
		},
		10,
		false,
		true)
	if err != nil {
		return err
	}

	sc.ReadRegistries(
		sc.RegistryContexts,
		// Read all registries recursively, because we want to delete every
		// image found in it (clearRepository works by deleting each image found
		// in sc.Inv).
		true,
		reg.MkReadRepositoryCmdReal)

	// Clear ALL registries in the test manifest. Blank slate!
	for _, rc := range t.Registries {
		fmt.Println("CLEARING REPO", rc.Name)
		clearRepository(rc.Name, &sc)
	}
	return nil
}

func getCmdEnableService(projectID, service string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"services",
		"enable",
		service,
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdListLogs(projectID string) []string {
	return []string{
		"gcloud",
		"logging",
		"logs",
		"list",
		fmt.Sprintf("--project=%s", projectID),
	}
}

// nolint[lll]
func getCmdDeleteLogs(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"logging",
		"logs",
		"delete",
		auditLogName,
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdDeleteErrorReportingEvents(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"beta",
		"error-reporting",
		"events",
		"delete",
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdListCloudRunServices(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"run",
		"services",
		"--platform=managed",
		"list",
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdDeleteCloudRunServices(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"run",
		"services",
		"--platform=managed",
		"delete",
		auditorName,
		fmt.Sprintf("--project=%s", projectID),
		"--region=us-central1",
	}
}

func getCmdListSubscriptions(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"pubsub",
		"subscriptions",
		"list",
		"--format=value(name)",
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdDeleteSubscription(projectID string) []string {
	return []string{
		"gcloud",
		"pubsub",
		"subscriptions",
		"delete",
		subscriptionName,
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdListTopics(projectID string) []string {
	return []string{
		"gcloud",
		"--quiet",
		"pubsub",
		"topics",
		"list",
		"--format=value(name)",
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdDeleteTopic(projectID, topic string) []string {
	return []string{
		"gcloud",
		"pubsub",
		"topics",
		"delete",
		topic,
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdCreateTopic(projectID, topic string) []string {
	return []string{
		"gcloud",
		"pubsub",
		"topics",
		"create",
		topic,
		fmt.Sprintf("--project=%s", projectID),
	}
}

// nolint[lll]
func getCmdEnablePubSubTokenCreation(
	projectNumber,
	projectID string,
) []string {
	return []string{
		"gcloud",
		"projects",
		"add-iam-policy-binding",
		projectID,
		fmt.Sprintf(
			"--member=serviceAccount:service-%s@gcp-sa-pubsub.iam.gserviceaccount.com",
			projectNumber),
		"--role=roles/iam.serviceAccountTokenCreator",
	}
}

func getCmdEmpowerServiceAccount(
	projectID, invokerServiceAccount string,
) []string {
	return []string{
		"gcloud",
		"run",
		"services",
		"add-iam-policy-binding",
		auditorName,
		fmt.Sprintf("--member=serviceAccount:%s", invokerServiceAccount),
		"--role=roles/run.invoker",
		"--platform=managed",
		fmt.Sprintf("--project=%s", projectID),
		"--region=us-central1",
	}
}

func getCmdPurgePubSubMessages(projectID string) []string {
	return []string{
		"gcloud",
		"pubsub",
		"subscriptions",
		"seek",
		subscriptionName,
		"--time=+p1y",
		fmt.Sprintf("--project=%s", projectID),
	}
}

func getCmdCloudRunPushEndpoint(projectID string) []string {
	return []string{
		"gcloud",
		"run",
		"services",
		"describe",
		auditorName,
		"--platform=managed",
		"--format=value(status.url)",
		fmt.Sprintf("--project=%s", projectID),
		"--region=us-central1",
	}
}

func getCmdCreatePubSubSubscription(
	projectID,
	pushEndpoint,
	invokerServiceAccount string,
) []string {
	return []string{
		"gcloud",
		"pubsub",
		"subscriptions",
		"create",
		subscriptionName,
		"--topic=gcr",
		"--expiration-period=never",
		fmt.Sprintf("--push-auth-service-account=%s", invokerServiceAccount),
		fmt.Sprintf("--push-endpoint=%s", pushEndpoint),
		fmt.Sprintf("--project=%s", projectID),
	}
}

// nolint[lll]
func getCmdShowLogs(projectID, uuid, pattern string) []string {
	fullLogName := fmt.Sprintf("projects/%s/logs/%s", projectID, auditLogName)
	uuidAndPattern := fmt.Sprintf("(%s) %s", uuid, pattern)
	return []string{
		"gcloud",
		"logging",
		"read",
		"--format=value(textPayload)",
		fmt.Sprintf("logName=%s resource.labels.project_id=%s %q", fullLogName, projectID, uuidAndPattern),
		fmt.Sprintf("--project=%s", projectID),
	}
}

const (
	subscriptionName    = "cip-auditor-test-invoker"
	auditorName         = "cip-auditor-test"
	auditLogName        = audit.LogName
	maxLogMatchAttempts = 10
)

// nolint[lll]
func getCmdsDeployCloudRun(
	pushRepo,
	projectID,
	manifestDir,
	uuid,
	invokerServiceAccount string,
) [][]string {
	auditorImg := fmt.Sprintf("%s/%s:latest", pushRepo, auditorName)
	return [][]string{
		{
			// Needs to run in Git repo root.
			"make",
			"image-push-cip-auditor-e2e",
		},
		{
			"gcloud",
			"run",
			"deploy",
			auditorName,
			fmt.Sprintf("--image=%s", auditorImg),
			fmt.Sprintf("--update-env-vars=%s,%s,%s",
				fmt.Sprintf("CIP_AUDIT_MANIFEST_REPO_MANIFEST_DIR=%s", manifestDir),
				fmt.Sprintf("CIP_AUDIT_GCP_PROJECT_ID=%s", projectID),
				// Generate a new UUID for this Cloud Run instance. Although the
				// Cloud Run instance gets a UUID assigned to it, using that
				// would require fetching it from within the instance which is
				// unnecessarily complicated. Instead we just generate one here
				// and thread it through to the instance.
				fmt.Sprintf("CIP_AUDIT_TESTCASE_UUID=%s", uuid),
			),
			"--platform=managed",
			"--no-allow-unauthenticated",
			"--region=us-central1",
			fmt.Sprintf("--project=%s", projectID),
			fmt.Sprintf("--service-account=%s", invokerServiceAccount),
		},
	}
}

func clearLogs(projectID string) error {
	args := getCmdListLogs(projectID)
	listCmd := command.New(args[0], args[1:]...)
	std, err := listCmd.RunSuccessOutput()
	if err != nil {
		return err
	}

	stdout := std.Output()
	if strings.Contains(stdout, auditLogName) {
		args = getCmdDeleteLogs(projectID)
		delCmd := command.New(args[0], args[1:]...)
		if err := delCmd.RunSuccess(); err != nil {
			return err
		}
	}

	return nil
}

func clearErrorReporting(projectID string) error {
	args := getCmdDeleteErrorReportingEvents(projectID)
	cmd := command.New(args[0], args[1:]...)
	return cmd.RunSuccess()
}

func clearCloudRun(projectID string) error {
	args := getCmdListCloudRunServices(projectID)
	cmd := command.New(args[0], args[1:]...)
	std, err := cmd.RunSuccessOutput()
	if err != nil {
		return err
	}

	stdout := std.Output()
	if strings.Contains(stdout, auditorName) {
		args = getCmdDeleteCloudRunServices(projectID)
		cmd := command.New(args[0], args[1:]...)
		if err := cmd.RunSuccess(); err != nil {
			return err
		}
	}

	return err
}

func deployCloudRun(
	repoRoot,
	pushRepo,
	manifestDir,
	projectID,
	uuid,
	invokerServiceAccount string,
) error {
	argss := getCmdsDeployCloudRun(
		pushRepo,
		projectID,
		manifestDir,
		uuid,
		invokerServiceAccount,
	)

	for _, args := range argss {
		cmd := command.NewWithWorkDir(repoRoot, args[0], args[1:]...)
		if err := cmd.RunSuccess(); err != nil {
			return err
		}
	}

	return nil
}

// clearSubscription deletes the existing subscription.
func clearSubscription(projectID string) error {
	args := getCmdListSubscriptions(projectID)
	cmd := command.New(args[0], args[1:]...)
	std, err := cmd.RunSuccessOutput()
	if err != nil {
		return err
	}

	stdout := std.Output()
	if strings.Contains(stdout, subscriptionName) {
		args = getCmdDeleteSubscription(projectID)
		cmd := command.New(args[0], args[1:]...)
		if err := cmd.RunSuccess(); err != nil {
			return err
		}
	}

	return nil
}

func clearPubSubTopic(projectID, topic string) error {
	args := getCmdListTopics(projectID)
	cmd := command.New(args[0], args[1:]...)
	std, err := cmd.RunSuccessOutput()
	if err != nil {
		return err
	}

	stdout := std.Output()
	if strings.Contains(stdout, topic) {
		args = getCmdDeleteTopic(projectID, topic)
		delCmd := command.New(args[0], args[1:]...)
		if err := delCmd.RunSuccess(); err != nil {
			return err
		}

		args = getCmdCreateTopic(projectID, topic)
		createCmd := command.New(args[0], args[1:]...)
		if err := createCmd.RunSuccess(); err != nil {
			return err
		}
	}

	return nil
}

func enablePubSubTokenCreation(
	projectNumber,
	projectID string,
) error {
	args := getCmdEnablePubSubTokenCreation(projectNumber, projectID)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func enableCloudResourceManagerAPI(
	projectID string,
) error {
	args := getCmdEnableService(
		projectID,
		"cloudresourcemanager.googleapis.com",
	)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func enableStackdriverAPI(
	projectID string,
) error {
	args := getCmdEnableService(
		projectID,
		"stackdriver.googleapis.com",
	)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func enableStackdriverErrorReportingAPI(
	projectID string,
) error {
	args := getCmdEnableService(
		projectID,
		"clouderrorreporting.googleapis.com",
	)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func enableCloudRunAPI(
	projectID string,
) error {
	args := getCmdEnableService(
		projectID,
		"run.googleapis.com",
	)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func enableServiceUsageAPI(
	projectID string,
) error {
	args := getCmdEnableService(
		projectID,
		"serviceusage.googleapis.com",
	)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func empowerServiceAccount(projectID, invokerServiceAccount string) error {
	args := getCmdEmpowerServiceAccount(projectID, invokerServiceAccount)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func clearPubSubMessages(projectID string) error {
	args := getCmdPurgePubSubMessages(projectID)
	cmd := command.New(args[0], args[1:]...)

	return cmd.RunSuccess()
}

func createPubSubSubscription(projectID, invokerServiceAccount string) error {
	args := getCmdCloudRunPushEndpoint(projectID)
	cloudRunCmd := command.New(args[0], args[1:]...)
	std, err := cloudRunCmd.RunSuccessOutput()
	if err != nil {
		return err
	}

	pushEndpoint := std.Output()
	args = getCmdCreatePubSubSubscription(
		projectID,
		strings.TrimSpace(pushEndpoint),
		invokerServiceAccount,
	)
	pubSubCmd := command.New(args[0], args[1:]...)

	return pubSubCmd.RunSuccess()
}

func runCheckedCommands(commands [][]string) error {
	// First ensure that all commands are allowable in the first place.
	for _, command := range commands {
		if err := checkCommand(command); err != nil {
			return err
		}
	}

	for _, c := range commands {
		klog.Infof("execing command %s", c)
		cmd := command.New(c[0], c[1:]...)
		if err := cmd.RunSuccess(); err != nil {
			return err
		}
	}

	return nil
}

func checkCommand(cmd []string) error {
	allowedCommands := [][]string{
		{"gcloud", "--quiet", "container", "images", "add-tag"},
	}

	allowedCommandsMap := make(map[string]bool)
	for _, allowedCommand := range allowedCommands {
		allowedCommandsMap[strings.Join(allowedCommand, "")] = true
	}

	// Reduce command to a single string.
	joined := strings.Join(cmd, "")

	for allowedCommand := range allowedCommandsMap {
		if strings.HasPrefix(joined, allowedCommand) {
			return nil
		}
	}

	return fmt.Errorf("command %s is not allowed; must be one of %s",
		cmd,
		allowedCommands)
}

func clearRepository(regName reg.RegistryName, sc *reg.SyncContext) {
	mkDeletionCmd := func(
		dest reg.RegistryContext,
		imageName reg.ImageName,
		digest reg.Digest,
	) stream.Producer {
		var sp stream.Subprocess
		sp.CmdInvocation = reg.GetDeleteCmd(
			dest,
			sc.UseServiceAccount,
			imageName,
			digest,
			true,
		)

		return &sp
	}

	sc.ClearRepository(regName, mkDeletionCmd, nil)
}

func checkLogs(projectID, uuid string, patterns []string) error {
	for _, pattern := range patterns {
		args := getCmdShowLogs(projectID, uuid, pattern)
		cmd := command.New(args[0], args[1:]...)
		std, err := cmd.RunSuccessOutput()
		fmt.Println(std.Output(), std.Error())
		if err != nil {
			return err
		}

		matched, stderr := std.Output(), std.Error()
		if err != nil {
			return err
		}

		// TODO: Is this required?
		if len(stderr) > 0 {
			return fmt.Errorf(
				"encountered stderr while searching logs: '%s'",
				stderr,
			)
		}

		if matched == "" {
			return fmt.Errorf("no matching log found for pattern '%s'", pattern)
		}
	}

	return nil
}

// E2ETest holds all the information about a single e2e test. It has the
// promoter manifest, and the before/after snapshots of all repositories that it
// cares about.
//
// SetupCip is the cip command to run to set up the state. If it is empty, cip
// is not called (to populate the GCR) --- this is useful for cases when we want
// to have a blank GCR.
//
// Registries is the list of all registries involved for this test case. To
// ensure hermeticity and reproducibility, these registries are *cleared* before
// any of the actual test logic is executed.
//
// SetupExtra is how the test environment is set up *before* the Cloud Run
// application is deployed.
//
// Mutations is how this test will modify the GCR state. It can be 1 or
// more CLI statements.
//
// List of log statements (strings) to find in the logs (they are exact
// string patterns to match, NOT regexes!). It is important to note that
// *all* GCR state changes will result in *some* sort of log from the
// auditor running in Cloud Run (whether the state change is VERIFIED or
// REJECTED).
type E2ETest struct {
	Name        string                `yaml:"name,omitempty"`
	Registries  []reg.RegistryContext `yaml:"registries,omitempty"`
	ManifestDir string                `yaml:"manifestDir,omitempty"`
	SetupCip    []string              `yaml:"setupCip,omitempty"`
	SetupExtra  [][]string            `yaml:"setupExtra,omitempty"`
	Mutations   [][]string            `yaml:"mutations,omitempty"`
	LogMatch    []string              `yaml:"logMatch,omitempty"`
}

// E2ETests is an array of E2ETest.
type E2ETests []E2ETest

func readE2ETests(filePath string) (E2ETests, error) {
	var ts E2ETests
	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return ts, err
	}
	if err := yaml.UnmarshalStrict(b, &ts); err != nil {
		return ts, err
	}

	return ts, nil
}

func printVersion() {
	klog.Infof("\n%s", version.Get().String())
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}
