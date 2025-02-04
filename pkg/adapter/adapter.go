/*
Copyright 2021 The Knative Authors

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

package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"go.uber.org/zap"
	ceph "knative.dev/eventing-ceph/pkg/apis/bindings/v1alpha1"
	"knative.dev/eventing/pkg/adapter/v2"
	"knative.dev/pkg/logging"
)

const (
	resourceGroup = "cephsources.sources.knative.dev"
)

type envConfig struct {
	adapter.EnvConfig

	// Port to listen incoming connections
	Port string `envconfig:"PORT"`
}

// cephReceiveAdapter converts incoming Ceph notifications to
// CloudEvents and then sends them to the specified Sink
type cephReceiveAdapter struct {
	logger    *zap.SugaredLogger
	client    cloudevents.Client
	port      string
	name      string
	namespace string
}

// NewEnvConfig function reads env variables defined in envConfig structure and
// returns accessor interface
func NewEnvConfig() adapter.EnvConfigAccessor {
	return &envConfig{}
}

// NewAdapter returns the instance of cephReceiveAdapter that implements adapter.Adapter interface
func NewAdapter(ctx context.Context, processed adapter.EnvConfigAccessor, ceClient cloudevents.Client) adapter.Adapter {
	logger := logging.FromContext(ctx)
	env := processed.(*envConfig)

	return &cephReceiveAdapter{
		logger:    logger,
		client:    ceClient,
		port:      env.Port,
		name:      env.Name,
		namespace: env.Namespace,
	}
}

// Start the ceph bucket notifications to knative adapter
func (ca *cephReceiveAdapter) Start(ctx context.Context) error {
	return ca.start(ctx.Done())
}

func (ca *cephReceiveAdapter) start(stopCh <-chan struct{}) error {
	http.HandleFunc("/", ca.postHandler)
	go http.ListenAndServe(":"+ca.port, nil)
	ca.logger.Info("Ceph to Knative adapter spawned HTTP server on port: " + ca.port)
	<-stopCh

	ca.logger.Info("Ceph to Knative adapter terminated")
	return nil
}

// postMessage convert bucket notifications to knative events and sent them to knative
func (ca *cephReceiveAdapter) postMessage(notification ceph.BucketNotification) error {
	eventTime, err := time.Parse(time.RFC3339, notification.EventTime)
	if err != nil {
		ca.logger.Infof("Failed to parse event timestamp, using local time. Error: %s", err.Error())
		eventTime = time.Now()
	}

	event := cloudevents.NewEvent()
	event.SetID(notification.ResponseElements.XAmzRequestID + notification.ResponseElements.XAmzID2)
	event.SetSource(notification.EventSource + "." + notification.AwsRegion + "." + notification.S3.Bucket.Name)
	event.SetType("com.amazonaws." + notification.EventName)
	event.SetSubject(notification.S3.Object.Key)
	event.SetTime(eventTime)
	err = event.SetData(cloudevents.ApplicationJSON, notification)
	if err != nil {
		return fmt.Errorf("failed to marshal event data: %w", err)
	}
	ctx := context.Background()
	metricTag := &adapter.MetricTag{
		Namespace:     ca.namespace,
		Name:          ca.name,
		ResourceGroup: resourceGroup,
	}
	ctx = adapter.ContextWithMetricTag(ctx, metricTag)

	return ca.sendCloudEvent(ctx, event)
}

// sendCloudEvent sends a cloudevent for a ceph notification.
func (ca *cephReceiveAdapter) sendCloudEvent(ctx context.Context, event cloudevents.Event) error {
	source := event.Context.GetSource()
	subject := event.Context.GetSubject()
	ca.logger.Debugf("sending cloudevent id: %s, source: %s, subject: %s", event.ID(), source, subject)

	if result := ca.client.Send(ctx, event); !cloudevents.IsACK(result) {
		ca.logger.Errorw("failed to send cloudevent", zap.Error(result), zap.String("source", source),
			zap.String("subject", subject), zap.String("id", event.ID()))
		return result
	}
	ca.logger.Debugf("cloudevent sent id: %s, source: %s, subject: %s", event.ID(), source, subject)
	return nil
}

// postHandler handles incoming bucket notifications from ceph
func (ca *cephReceiveAdapter) postHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	if r.Method != "POST" {
		ca.logger.Infof("%s method not allowed", r.Method)
		http.Error(w, "405 Method Not Allowed", http.StatusBadRequest)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		ca.logger.Infof("Error reading message body: %s", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var notifications ceph.BucketNotifications
	err = json.Unmarshal(body, &notifications)

	if err != nil {
		ca.logger.Infof("Failed to parse JSON: %s", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ca.logger.Debugf("%d events found in message", len(notifications.Records))
	for _, notification := range notifications.Records {
		ca.logger.Debugf("Received Ceph bucket notification: %+v", notification)
		if err := ca.postMessage(notification); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
}
