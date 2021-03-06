package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/engine-api/types/events"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/secrets-bridge/writer"
)

type VaultResponseThing struct {
	ExternalId string
	TempToken  string
	CubbyPath  string
}

type JsonHandler struct {
	metadataCli           *metadata.Client
	remoteVerificationUrl string
	agentUUID             string
	signingKey            string
}

type MessageHandler interface {
	Handle(*events.Message) error
}

func NewMessageHandler(opts map[string]interface{}) (MessageHandler, error) {
	handler := &JsonHandler{}

	mdUrl, ok := opts["metadata-url"]
	if !ok {
		return handler, errors.New("No metadataURL defined")
	}

	client, err := metadata.NewClientAndWait(mdUrl.(string))
	if err != nil {
		return handler, err
	}
	handler.metadataCli = client

	selfContainer, err := handler.metadataCli.GetSelfContainer()
	if err != nil {
		return handler, err
	}

	handler.agentUUID = selfContainer.UUID

	rsUrl, ok := opts["bridge-url"]
	if !ok || rsUrl.(string) == "" {
		return handler, errors.New("No bridge URL defined")
	}

	handler.signingKey = os.Getenv("CATTLE_SECRET_KEY")
	if handler.signingKey == "" {
		return handler, errors.New("No signing key available.")
	}

	handler.remoteVerificationUrl = rsUrl.(string)

	return handler, nil
}

func (j *JsonHandler) Handle(msg *events.Message) error {
	message, err := j.buildRequestMessage(msg)
	if err != nil {
		return err
	}

	jMsg, err := json.Marshal(message)
	if err != nil {
		return err
	}

	b := bytes.NewBuffer(jMsg)

	resp, err := j.postRequestToSecretBridge(b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return fmt.Errorf("Didn't get created response, got: %d", resp.StatusCode)
	}

	var vaultThing VaultResponseThing
	decoder := json.NewDecoder(resp.Body)
	decoder.Decode(&vaultThing)

	logrus.Debugf("Got Response: %#v", vaultThing)

	err = writeResponse(&vaultThing)
	if err != nil {
		logrus.Errorf("Error: writing response to %s", vaultThing.ExternalId)
		logrus.Error(err)
		return err
	}

	return nil
}

func (j *JsonHandler) buildRequestMessage(msg *events.Message) (*ContainerEventMessage, error) {
	message := &ContainerEventMessage{
		ContainerType: "cattle",
	}

	logrus.Debugf("Received action: %s, from container: %s", msg.Action, msg.ID)

	if _, ok := msg.Actor.Attributes["io.kubernetes.pod.namespace"]; ok {
		logrus.Debugf("Container type is Kubernetes")

		if name, ok := msg.Actor.Attributes["io.kubernetes.container.name"]; ok && name == "POD" {
			return message, errors.New("Ignoring K8s POD container")
		}

		if !j.checkForK8sSecretsLabel(msg) {
			return message, errors.New("Secrets bridge label not found")
		}
		message.ContainerType = "kubernetes"
	}

	if message.ContainerType == "cattle" {
		if val, ok := msg.Actor.Attributes["secrets.bridge.enabled"]; !ok || val != "true" {
			return message, errors.New("Secrets bridge not enabled")
		}
	}

	message.Event = msg
	message.Action = msg.Action

	err := message.SetUUIDFromMetadata(j.metadataCli)
	if err != nil {
		return message, err
	}

	message.Host, err = os.Hostname()
	if err != nil {
		return message, err
	}

	logrus.Debugf("Packaged Message: %#v", message)

	return message, nil
}

func (j *JsonHandler) generateSignatureHeader() string {
	mac := hmac.New(sha256.New, []byte(j.signingKey))

	logrus.Debugf("UUID: %s", j.agentUUID)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	logrus.Debugf("Time: %s", ts)

	mac.Write([]byte(j.agentUUID + ts))
	hmacMessage := string(mac.Sum(nil)[:mac.Size()])
	logrus.Debugf("hmac: %x", hmacMessage)

	// UUID:TIMESTAMP:SIGNATURE
	message := strings.Join([]string{j.agentUUID, ts, hmacMessage}, ":")

	return base64.StdEncoding.EncodeToString([]byte(message))
}

func (j *JsonHandler) postRequestToSecretBridge(buffer *bytes.Buffer) (*http.Response, error) {
	client := &http.Client{}
	req, err := http.NewRequest("POST", j.remoteVerificationUrl, buffer)
	if err != nil {
		return nil, err
	}

	req.Header.Add("X-Agent-Signature", j.generateSignatureHeader())

	return client.Do(req)
}

func writeResponse(message *VaultResponseThing) error {
	cli, err := getDockerClient()
	if err != nil {
		logrus.Fatal(err)
	}

	opts := map[string]interface{}{
		"dockerClient": cli,
		"message":      formatMessage(message),
		"path":         "/tmp",
		"containerId":  message.ExternalId,
	}

	writer, err := writer.NewSecretWriter(opts)
	if err != nil {
		return err
	}

	return writer.Write()
}

func formatMessage(message *VaultResponseThing) string {
	return fmt.Sprintf("export CUBBY_PATH=%s\nexport TEMP_TOKEN=%s\n", message.CubbyPath, message.TempToken)
}

func (j *JsonHandler) checkForK8sSecretsLabel(msg *events.Message) bool {
	enabled := false
	labelExists := false
	labelValue := "notavailable"

	maxWait := 129 * time.Second

	name := msg.Actor.Attributes["io.kubernetes.pod.name"]
	nameSpace := msg.Actor.Attributes["io.kubernetes.pod.namespace"]

	logrus.Debugf("Pod Name: %s", name)
	logrus.Debugf("Pod Namespace: %s", name)

	container := metadata.Container{}
	for i := 1 * time.Second; i < maxWait; i *= time.Duration(2) {
		containers, err := j.metadataCli.GetContainers()
		if err != nil {
			logrus.Error(err)
			return false
		}
		container = loopContainers(name, "io.kubernetes.pod.namespace", nameSpace, containers)
		if labelValue, labelExists = container.Labels["secrets.bridge.enabled"]; labelExists {
			break
		}
		time.Sleep(i)
	}

	logrus.Debugf("Labels found: %#v", container.Labels)

	if labelValue == "true" {
		enabled = true
	}

	return enabled
}
