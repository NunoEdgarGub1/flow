package actor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/AsynkronIT/protoactor-go/actor"
	"github.com/fnproject/flow/blobs"
	"github.com/fnproject/flow/model"
	"github.com/fnproject/flow/protocol"
	"github.com/golang/protobuf/jsonpb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

const fnCallIDHeader = "Fn_call_id"

var (
	activeFnCallsMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "flow_concurrent_active_fn_calls",
		Help: "Current number of fn calls waiting for a response.",
	})
)

func init() {
	prometheus.MustRegister(activeFnCallsMetric)
	activeFnCallsMetric.Set(0.0)
}

type graphExecutor struct {
	faasAddr  string
	client    httpClient
	blobStore blobs.Store
	log       *logrus.Entry
}

// For mocking
type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

// ExecHandler abstracts the FaaS execution backend
// implementations must handle all errors and return an appropriate invocation responser
type ExecHandler interface {
	HandleInvokeStage(msg *model.InvokeStageRequest) *model.FaasInvocationResponse
	HandleInvokeFunction(msg *model.InvokeFunctionRequest) *model.FaasInvocationResponse
}

// NewExecutor creates a new executor actor with the given funcions service endpoint
func NewExecutor(faasAddress string, blobStore blobs.Store) actor.Actor {
	client := &http.Client{}
	// TODO configure timeouts
	client.Timeout = 300 * time.Second

	return &graphExecutor{faasAddr: faasAddress,
		log:       logrus.WithField("logger", "executor_actor").WithField("faas_url", faasAddress),
		client:    client,
		blobStore: blobStore,
	}
}

func (exec *graphExecutor) Receive(context actor.Context) {
	sender := context.Sender()
	switch msg := context.Message().(type) {
	case *actor.Started:
		exec.log.Info("Started executor actor")
	case *model.InvokeStageRequest:
		go func() { sender.Tell(exec.HandleInvokeStage(msg)) }()
	case *model.InvokeFunctionRequest:
		go func() { sender.Tell(exec.HandleInvokeFunction(msg)) }()
	}
}

func (exec *graphExecutor) HandleInvokeStage(msg *model.InvokeStageRequest) *model.FaasInvocationResponse {
	stageLog := exec.log.WithFields(logrus.Fields{"flow_id": msg.FlowId, "stage_id": msg.StageId, "function_id": msg.FunctionId})
	stageLog.Info("Running Stage")

	runtimeRequest := &model.RuntimeInvokeStageRequest{
		FlowId:  msg.GetFlowId(),
		StageId: msg.GetStageId(),
		Args:    msg.GetArgs(),
		Closure: msg.GetClosure(),
	}
	buf := new(bytes.Buffer)
	writer := jsonpb.Marshaler{EmitDefaults: true, OrigName: true}
	err := writer.Marshal(buf, runtimeRequest)
	if err != nil {
		return stageFailed(msg, model.ErrorDatumType_stage_failed, "Could not marshal the runtime invoke stage request message.", "")
	}

	req, _ := http.NewRequest("POST", exec.faasAddr+"/"+msg.FunctionId, buf)
	req.Header.Set("Content-type", "application/json")
	req.Header.Set(protocol.HeaderFlowID, msg.FlowId)
	req.Header.Set(protocol.HeaderStageRef, msg.StageId)

	activeFnCallsMetric.Inc()
	defer activeFnCallsMetric.Dec()
	resp, err := exec.client.Do(req)

	if err != nil {
		return stageFailed(msg, model.ErrorDatumType_stage_failed, "HTTP error on stage invocation: Can the flow service talk to the functions server?", "")
	}
	defer resp.Body.Close()

	lbDelayHeader := resp.Header.Get("Xxx-Fxlb-Wait")
	callID := resp.Header.Get(fnCallIDHeader)

	if !exec.successfulResponse(resp) {
		stageLog.WithField("fn_call_id", callID).WithField("fn_lb_delay", lbDelayHeader).WithField("http_status", fmt.Sprintf("%d", resp.StatusCode)).Error("Got non-200 error from FaaS endpoint")

		if resp.StatusCode == 504 {
			return &model.FaasInvocationResponse{FlowId: msg.FlowId, StageId: msg.StageId, FunctionId: msg.FunctionId, Result: model.NewInternalErrorResult(model.ErrorDatumType_stage_timeout, "stage timed out"), CallId: callID}
		}
		return stageFailed(msg, model.ErrorDatumType_stage_failed, fmt.Sprintf("Invalid http response from functions platform code %d", resp.StatusCode), callID)
	}

	runtimeResponse := &model.RuntimeInvokeStageResponse{}
	reader := jsonpb.Unmarshaler{AllowUnknownFields: true}
	err = reader.Unmarshal(resp.Body, runtimeResponse)
	if err != nil {
		stageLog.WithField("fn_call_id", callID).WithField("fn_lb_delay", lbDelayHeader).Error("Failed to read result from functions service: ", err)
		return stageFailed(msg, model.ErrorDatumType_invalid_stage_response, "Failed to read result from functions service", callID)
	}

	result := runtimeResponse.GetResult()
	if result == nil {
		stageLog.WithField("fn_call_id", callID).WithField("fn_lb_delay", lbDelayHeader).Error("Empty result from functions service")
		return stageFailed(msg, model.ErrorDatumType_invalid_stage_response, "Empty result from functions service", callID)
	}

	stageLog.WithField("fn_call_id", callID).WithField("fn_lb_delay", lbDelayHeader).WithField("successful", fmt.Sprintf("%t", result.Successful)).Info("Got stage response")

	return &model.FaasInvocationResponse{FlowId: msg.FlowId, StageId: msg.StageId, FunctionId: msg.FunctionId, Result: result, CallId: callID}
}

func stageFailed(msg *model.InvokeStageRequest, errorType model.ErrorDatumType, errorMessage string, callID string) *model.FaasInvocationResponse {
	return &model.FaasInvocationResponse{FlowId: msg.FlowId, StageId: msg.StageId, FunctionId: msg.FunctionId, Result: model.NewInternalErrorResult(errorType, errorMessage), CallId: callID}
}

func (exec *graphExecutor) HandleInvokeFunction(msg *model.InvokeFunctionRequest) *model.FaasInvocationResponse {
	datum := msg.Arg

	method := strings.ToUpper(model.HTTPMethod_name[int32(datum.Method)])
	stageLog := exec.log.WithFields(logrus.Fields{"flow_id": msg.FlowId, "stage_id": msg.StageId, "target_function_id": msg.FunctionId, "method": method})
	stageLog.Info("Sending function invocation")

	var bodyReader io.Reader

	if datum.Body != nil {
		var err error
		bodyReader, err = exec.blobStore.Read(msg.FlowId, datum.Body.BlobId)
		if err != nil {
			stageLog.WithError(err).Warn("Failed to fetch blob from store")
			return exec.invokeFailed(msg, "Failed to read data for invocation", "")
		}
	} else {
		bodyReader = http.NoBody
	}

	req, err := http.NewRequest(strings.ToUpper(method), exec.faasAddr+"/"+msg.FunctionId, bodyReader)
	if err != nil {
		exec.log.Error("Failed to create http request:", err)
		return exec.invokeFailed(msg, "Failed to create HTTP request", "")
	}

	if datum.Body != nil {
		req.Header.Set("Content-Type", datum.Body.ContentType)
	}

	for _, header := range msg.Arg.Headers {
		req.Header.Add(header.Key, header.Value)
	}

	activeFnCallsMetric.Inc()
	defer activeFnCallsMetric.Dec()
	resp, err := exec.client.Do(req)

	if err != nil {
		exec.log.Error("Http error calling functions service:", err)
		return exec.invokeFailed(msg, "Failed to call function", "")

	}
	defer resp.Body.Close()

	lbDelayHeader := resp.Header.Get("Xxx-Fxlb-Wait")
	if len(lbDelayHeader) > 0 {
		stageLog.WithField("fn_lb_delay", lbDelayHeader).Info("Fn load balancer delay")
	} else {
		stageLog.Info("No Fn load balancer delay header received")
	}

	callID := resp.Header.Get(fnCallIDHeader)

	var contentType = resp.Header.Get("Content-type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var headers = make([]*model.HTTPHeader, 0)
	for headerName, valList := range resp.Header {
		// Don't copy content type into headers
		if textproto.CanonicalMIMEHeaderKey(headerName) == "Content-Type" {
			continue
		}
		for _, val := range valList {
			headers = append(headers, &model.HTTPHeader{Key: headerName, Value: val})
		}
	}

	blob, err := exec.blobStore.Create(msg.FlowId, contentType, resp.Body)
	if err != nil {
		stageLog.WithError(err).Warn("failed to persist data in blob store")
		return exec.invokeFailed(msg, "Failed to persist HTTP response data", callID)
	}

	resultDatum := &model.Datum{
		Val: &model.Datum_HttpResp{
			HttpResp: &model.HTTPRespDatum{
				Headers:    headers,
				Body:       model.BlobDatumFromBlobStoreBlob(blob),
				StatusCode: int32(resp.StatusCode)}}}

	result := &model.CompletionResult{Successful: exec.successfulResponse(resp), Datum: resultDatum}
	return &model.FaasInvocationResponse{FlowId: msg.FlowId, StageId: msg.StageId, FunctionId: msg.FunctionId, Result: result, CallId: callID}
}

func (exec *graphExecutor) successfulResponse(resp *http.Response) bool {
	// assume any non-error codes are successful
	// TODO doc in spec
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func (exec *graphExecutor) invokeFailed(msg *model.InvokeFunctionRequest, errorMessage string, callID string) *model.FaasInvocationResponse {

	return &model.FaasInvocationResponse{FlowId: msg.FlowId, StageId: msg.StageId, FunctionId: msg.FunctionId, Result: model.NewInternalErrorResult(model.ErrorDatumType_function_invoke_failed, errorMessage), CallId: callID}
}
