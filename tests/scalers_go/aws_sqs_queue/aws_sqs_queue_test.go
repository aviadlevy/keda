//go:build e2e
// +build e2e

package aws_sqs_queue_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"

	. "github.com/kedacore/keda/v2/tests/helper"
)

// Load environment variables from .env file
var _ = godotenv.Load("../../.env")

const (
	testName = "aws-sqs-queue-test"
)

type templateData struct {
	TestNamespace      string
	DeploymentName     string
	ScaledObjectName   string
	SecretName         string
	AwsAccessKeyID     string
	AwsSecretAccessKey string
	AwsRegion          string
	SqsQueue           string
}

type templateValues map[string]string

const (
	secretTemplate = `apiVersion: v1
kind: Secret
metadata:
  name: {{.SecretName}}
  namespace: {{.TestNamespace}}
data:
  AWS_ACCESS_KEY_ID: {{.AwsAccessKeyID}}
  AWS_SECRET_ACCESS_KEY: {{.AwsSecretAccessKey}}
`

	triggerAuthenticationTemplate = `apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: keda-trigger-auth-aws-credentials
  namespace: {{.TestNamespace}}
spec:
  secretTargetRef:
  - parameter: awsAccessKeyID     # Required.
    name: {{.SecretName}}         # Required.
    key: AWS_ACCESS_KEY_ID        # Required.
  - parameter: awsSecretAccessKey # Required.
    name: {{.SecretName}}         # Required.
    key: AWS_SECRET_ACCESS_KEY    # Required.
`

	deploymentTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.DeploymentName}}
  namespace: {{.TestNamespace}}
  labels:
    app: {{.DeploymentName}}
spec:
  replicas: 0
  selector:
    matchLabels:
      app: {{.DeploymentName}}
  template:
    metadata:
      labels:
        app: {{.DeploymentName}}
    spec:
      containers:
      - name: nginx
        image: nginx:1.14.2
        ports:
        - containerPort: 80
`

	scaledObjectTemplate = `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: {{.ScaledObjectName}}
  namespace: {{.TestNamespace}}
  labels:
    app: {{.DeploymentName}}
spec:
  scaleTargetRef:
    name: {{.DeploymentName}}
  maxReplicaCount: 2
  minReplicaCount: 0
  cooldownPeriod: 1
  triggers:
    - type: aws-sqs-queue
      authenticationRef:
        name: keda-trigger-auth-aws-credentials
      metadata:
        awsRegion: {{.AwsRegion}}
        queueURL: {{.SqsQueue}}
        queueLength: "1"
`
)

var (
	testNamespace      = fmt.Sprintf("%s-ns", testName)
	deploymentName     = fmt.Sprintf("%s-deployment", testName)
	scaledObjectName   = fmt.Sprintf("%s-so", testName)
	secretName         = fmt.Sprintf("%s-secret", testName)
	sqsQueueName       = fmt.Sprintf("%s-keda-queue", testName)
	awsAccessKeyID     = os.Getenv("AWS_ACCESS_KEY")
	awsSecretAccessKey = os.Getenv("AWS_SECRET_KEY")
	awsRegion          = os.Getenv("AWS_REGION")
	maxReplicaCount    = 2
	minReplicaCount    = 0
)

func TestSqsScaler(t *testing.T) {
	// setup SQS
	sqsClient := createSqsClient()
	queue := createSqsQueue(t, sqsClient)

	// Create kubernetes resources
	kc := GetKubernetesClient(t)
	data, templates := getTemplateData(*queue.QueueUrl)
	CreateKubernetesResources(t, kc, testNamespace, data, templates)

	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, minReplicaCount, 60, 1),
		"replica count should be 0 after a minute")

	// test scaling
	testScaleUp(t, kc, sqsClient, queue.QueueUrl)
	testScaleDown(t, kc, sqsClient, queue.QueueUrl)

	// cleanup
	DeleteKubernetesResources(t, kc, testNamespace, data, templates)
	cleanupQueue(t, sqsClient, queue.QueueUrl)
}

func testScaleUp(t *testing.T, kc *kubernetes.Clientset, sqsClient *sqs.SQS, queueURL *string) {
	t.Log("--- testing scale up ---")
	for i := 0; i < 10; i++ {
		msg := fmt.Sprintf("Message - %d", i)
		_, err := sqsClient.SendMessageWithContext(context.Background(), &sqs.SendMessageInput{
			QueueUrl:     queueURL,
			MessageBody:  aws.String(msg),
			DelaySeconds: aws.Int64(10),
		})
		assert.NoErrorf(t, err, "cannot send message - %s", err)
	}

	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, maxReplicaCount, 180, 1),
		"replica count should be 2 after 3 minutes")
}

func testScaleDown(t *testing.T, kc *kubernetes.Clientset, sqsClient *sqs.SQS, queueURL *string) {
	t.Log("--- testing scale down ---")
	_, err := sqsClient.PurgeQueueWithContext(context.Background(), &sqs.PurgeQueueInput{
		QueueUrl: queueURL,
	})
	assert.NoErrorf(t, err, "cannot clear queue - %s", err)

	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, minReplicaCount, 180, 1),
		"replica count should be 0 after 3 minutes")
}

func createSqsQueue(t *testing.T, sqsClient *sqs.SQS) *sqs.CreateQueueOutput {
	queue, err := sqsClient.CreateQueueWithContext(context.Background(), &sqs.CreateQueueInput{
		QueueName: &sqsQueueName,
		Attributes: map[string]*string{
			"DelaySeconds":           aws.String("60"),
			"MessageRetentionPeriod": aws.String("86400"),
		}})
	assert.NoErrorf(t, err, "failed to create queue - %s", err)
	return queue
}

func cleanupQueue(t *testing.T, sqsClient *sqs.SQS, queueURL *string) {
	t.Log("--- cleaning up ---")
	_, err := sqsClient.DeleteQueueWithContext(context.Background(), &sqs.DeleteQueueInput{
		QueueUrl: queueURL,
	})
	assert.NoErrorf(t, err, "cannot delete queue - %s", err)
}

func createSqsClient() *sqs.SQS {
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(awsRegion),
	}))

	return sqs.New(sess, &aws.Config{
		Region:      aws.String(awsRegion),
		Credentials: credentials.NewStaticCredentials(awsAccessKeyID, awsSecretAccessKey, ""),
	})
}

func getTemplateData(sqsQueue string) (templateData, templateValues) {
	return templateData{
		TestNamespace:      testNamespace,
		DeploymentName:     deploymentName,
		ScaledObjectName:   scaledObjectName,
		SecretName:         secretName,
		AwsAccessKeyID:     base64.StdEncoding.EncodeToString([]byte(awsAccessKeyID)),
		AwsSecretAccessKey: base64.StdEncoding.EncodeToString([]byte(awsSecretAccessKey)),
		AwsRegion:          awsRegion,
		SqsQueue:           sqsQueue,
	}, templateValues{"secretTemplate": secretTemplate, "triggerAuthenticationTemplate": triggerAuthenticationTemplate, "deploymentTemplate": deploymentTemplate, "scaledObjectTemplate": scaledObjectTemplate}
}
