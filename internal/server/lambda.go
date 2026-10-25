package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// StartLambdaHandler starts the AWS Lambda handler.
func StartLambdaHandler(app *Application) {
	log.Println("Starting Lambda handler mode...")
	lambda.Start(func(ctx context.Context, rawEvent json.RawMessage) (any, error) {
		return app.HandleLambdaEvent(ctx, rawEvent)
	})
}

// HandleLambdaEvent processes any Lambda event type.
func (app *Application) HandleLambdaEvent(ctx context.Context, rawEvent json.RawMessage) (any, error) {
	// Ensure database connection is established (lazy initialization)
	// Safe to call on every request - mutex guards connection and allows retry on transient failures
	if err := app.ensureDB(ctx); err != nil {
		log.Printf("Failed to establish database connection: %v", err)
		return nil, fmt.Errorf("database connection failed: %w", err)
	}

	eventType := detectLambdaEventType(rawEvent)
	log.Printf("Received %s event (size: %d bytes)", eventType, len(rawEvent))

	switch eventType {
	case "http":
		return app.handleLambdaHTTPEvent(ctx, rawEvent)
	case "sqs":
		return app.handleLambdaSQSEvent(ctx, rawEvent)
	case "scheduled":
		return app.handleLambdaScheduledEvent(ctx, rawEvent)
	default:
		// Return a distinct error instead of silently treating an unrecognized
		// payload as a scheduled event. Masking the event shape as "unknown
		// scheduled task action" makes the real cause hard to diagnose (04-N4).
		return nil, fmt.Errorf("unrecognized Lambda event shape (size %d bytes); not an HTTP/SQS/scheduled event", len(rawEvent))
	}
}

// detectLambdaEventType determines the type of Lambda event.
func detectLambdaEventType(rawEvent json.RawMessage) string {
	// Check for Lambda Function URL / API Gateway event
	var httpEvent struct {
		RequestContext struct {
			HTTP struct {
				Method string `json:"method"`
			} `json:"http"`
		} `json:"requestContext"`
		HTTPMethod string `json:"httpMethod"`
	}
	if err := json.Unmarshal(rawEvent, &httpEvent); err == nil {
		if httpEvent.RequestContext.HTTP.Method != "" || httpEvent.HTTPMethod != "" {
			return "http"
		}
	}

	// Check for SQS event
	var sqsEvent struct {
		Records []struct {
			EventSource string `json:"eventSource"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(rawEvent, &sqsEvent); err == nil {
		if len(sqsEvent.Records) > 0 && sqsEvent.Records[0].EventSource == "aws:sqs" {
			return "sqs"
		}
	}

	// Check for EventBridge scheduled event
	var scheduledEvent struct {
		Source     string `json:"source"`
		DetailType string `json:"detail-type"`
		Action     string `json:"action"`
	}
	if err := json.Unmarshal(rawEvent, &scheduledEvent); err == nil {
		if scheduledEvent.Source == "aws.events" || scheduledEvent.Action != "" {
			return "scheduled"
		}
	}

	return "unknown"
}

// handleLambdaHTTPEvent processes HTTP requests from Lambda Function URL.
// When STATIC_DIR is set, serves static files for non-API paths directly
// from the container filesystem, enabling Lambda Function URL to serve
// the full frontend without S3/CloudFront.
func (app *Application) handleLambdaHTTPEvent(ctx context.Context, rawEvent json.RawMessage) (*events.LambdaFunctionURLResponse, error) {
	var request events.LambdaFunctionURLRequest
	if err := json.Unmarshal(rawEvent, &request); err != nil {
		log.Printf("Failed to parse HTTP event: %v", err)
		headers := lambdaSecurityHeaders()
		headers["Content-Type"] = "application/json"
		return &events.LambdaFunctionURLResponse{
			StatusCode: 400,
			Body:       `{"error": "Invalid request format"}`,
			Headers:    headers,
		}, nil
	}

	// Lambda Function URL base64-encodes the request body whenever its
	// Content-Type isn't recognized as text (the inbound mirror of the
	// isTextContentType decision this file already makes for outbound
	// responses). Decode it here, once, before anything downstream (OIDC,
	// static files, the API router) reads request.Body -- otherwise handlers
	// that parse JSON or form-urlencoded bodies (e.g. resolveApprovalToken)
	// see a base64 blob instead of plaintext.
	if request.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(request.Body)
		if err != nil {
			log.Printf("Failed to base64-decode request body: %v", err)
			headers := lambdaSecurityHeaders()
			headers["Content-Type"] = "application/json"
			return &events.LambdaFunctionURLResponse{
				StatusCode: 400,
				Body:       `{"error": "Invalid request format"}`,
				Headers:    headers,
			}, nil
		}
		request.Body = string(decoded)
		request.IsBase64Encoded = false
	}

	// Intercept OIDC issuer endpoints (discovery + JWKS, served under
	// /oidc/) before both the static-file fallback and the main API
	// router. api.HandleOIDC does its own auth-less dispatch so the
	// issuer paths don't leak into the API router tables or the
	// security middleware.
	if resp, handled := app.API.HandleOIDC(ctx, &request); handled {
		return resp, nil
	}

	// Serve static files for non-API paths when STATIC_DIR is configured
	reqPath := request.RawPath
	if app.staticDir != "" && isStaticPath(reqPath) {
		return app.serveLambdaStatic(reqPath)
	}

	resp, err := app.API.HandleRequest(ctx, &request)
	if err == nil {
		compressLambdaResponse(&request, resp)
	}
	return resp, err
}

// lambdaSecurityHeaders returns the standard security headers for Lambda responses.
// These match the securityHeaders() middleware used in HTTP mode. The
// Content-Security-Policy is what actually enforces frame-ancestors 'none'
// against clickjacking — browsers ignore frame-ancestors in a <meta> tag,
// so relying on the frontend's meta CSP alone would leave Lambda deploys
// unprotected.
func lambdaSecurityHeaders() map[string]string {
	return map[string]string{
		"Strict-Transport-Security": api.HSTSHeaderValue,
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Permissions-Policy":        "camera=(), microphone=(), geolocation=()",
		"Content-Security-Policy":   htmlCSP,
	}
}

// serveLambdaStatic serves a static file as a Lambda Function URL response.
func (app *Application) serveLambdaStatic(urlPath string) (*events.LambdaFunctionURLResponse, error) {
	content, contentType, cacheControl, found := serveStaticForLambda(app.staticDir, urlPath)
	if !found {
		headers := lambdaSecurityHeaders()
		headers["Content-Type"] = "text/plain"
		return &events.LambdaFunctionURLResponse{
			StatusCode: 404,
			Body:       "Not Found",
			Headers:    headers,
		}, nil
	}

	headers := lambdaSecurityHeaders()
	headers["Content-Type"] = contentType
	headers["Cache-Control"] = cacheControl

	// Text-based content types can be sent as plain body
	if isTextContentType(contentType) {
		return &events.LambdaFunctionURLResponse{
			StatusCode:      200,
			Body:            string(content),
			IsBase64Encoded: false,
			Headers:         headers,
		}, nil
	}

	// Binary content must be base64-encoded
	return &events.LambdaFunctionURLResponse{
		StatusCode:      200,
		Body:            base64.StdEncoding.EncodeToString(content),
		IsBase64Encoded: true,
		Headers:         headers,
	}, nil
}

// isTextContentType returns true for content types that can be sent as plain text
// in Lambda Function URL responses (not base64-encoded).
func isTextContentType(ct string) bool {
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	textTypes := []string{
		"application/json",
		"application/javascript",
		"application/xml",
		"image/svg+xml",
	}
	for _, t := range textTypes {
		if strings.HasPrefix(ct, t) {
			return true
		}
	}
	return false
}

// handleLambdaSQSEvent processes SQS messages (for async purchase processing).
//
// When app.appConfig.ReportSQSBatchItemFailures is true, returns a partial
// batch response (events.SQSEventResponse) naming only the records that
// actually failed, with a nil error, instead of a single error for the
// whole batch (issue #108). Lambda's SQS integration treats ANY non-nil
// handler error as "the entire batch failed" and redelivers every record,
// including ones HandleSQSMessage already processed successfully. On
// redelivery those already-succeeded records typically fail a second time
// (status already advanced past what the CAS-guarded handlers expect),
// which redelivers the whole batch again and burns the redrive budget on
// records that were never actually broken, until the one genuinely poison
// message drags the rest of the batch to the DLQ with it.
//
// The partial-response shape is only honored by AWS when the SQS
// event-source mapping sets function_response_types =
// ["ReportBatchItemFailures"] (Terraform; see loadReportSQSBatchItemFailures
// for why no such resource currently exists in this repo). Without that
// setting live, AWS ignores the response body entirely and treats a nil
// error as the WHOLE BATCH having succeeded -- deleting every message from
// the queue, including the ones that just failed. That is a strictly worse
// outcome than the old all-or-nothing behavior (which at least redelivered
// and eventually DLQ'd a genuine failure): a transient error would now
// silently drop an approval, and a poison message would vanish instead of
// landing in the DLQ for investigation. ReportSQSBatchItemFailures defaults
// to false specifically to keep the old, safe, all-or-nothing behavior
// until an operator has confirmed the event source mapping is actually
// configured to honor partial responses and flips
// SQS_REPORT_BATCH_ITEM_FAILURES=true.
//
// Regardless of the flag, a batch where every record failed always returns
// an error: there is no reason to ack (or partially ack) a batch that
// accomplished nothing, flag or no flag.
func (app *Application) handleLambdaSQSEvent(ctx context.Context, rawEvent json.RawMessage) (any, error) {
	var sqsEvent events.SQSEvent
	if err := json.Unmarshal(rawEvent, &sqsEvent); err != nil {
		log.Printf("Failed to parse SQS event: %v", err)
		return nil, err
	}

	failures := []events.SQSBatchItemFailure{}
	for _rvc := range sqsEvent.Records {
		record := sqsEvent.Records[_rvc]
		log.Printf("Processing SQS message: %s", record.MessageId)
		if err := app.HandleSQSMessage(ctx, record.Body); err != nil {
			log.Printf("Failed to process message %s: %v", record.MessageId, err)
			failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}

	if len(failures) == 0 {
		return events.SQSEventResponse{BatchItemFailures: failures}, nil
	}

	if len(failures) == len(sqsEvent.Records) {
		return nil, fmt.Errorf("failed to process all %d SQS message(s) in batch: %v", len(failures), sqsBatchItemFailureIDs(failures))
	}

	if !app.appConfig.ReportSQSBatchItemFailures {
		// Fail closed: see the function doc comment above for why acking
		// (even partially) without the Terraform side confirmed live would
		// silently drop failed messages instead of redelivering them.
		return nil, fmt.Errorf("failed to process %d SQS message(s): %v", len(failures), sqsBatchItemFailureIDs(failures))
	}

	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

// sqsBatchItemFailureIDs extracts the message IDs from a batch item failure
// list for the aggregate-error log/error text.
func sqsBatchItemFailureIDs(failures []events.SQSBatchItemFailure) []string {
	ids := make([]string, len(failures))
	for i, f := range failures {
		ids[i] = f.ItemIdentifier
	}
	return ids
}

// loadReportSQSBatchItemFailures parses SQS_REPORT_BATCH_ITEM_FAILURES at
// startup (called once from NewApplication). Unset means false: keep the
// old all-or-nothing batch behavior, which is safe (if noisier) until an
// operator confirms the SQS event source mapping's function_response_types
// actually includes "ReportBatchItemFailures" -- a Terraform resource that
// does not exist anywhere in this repo today (issue #108 follow-up: no
// aws_sqs_queue / AWS::SQS::Queue / EventSourceMapping / batch_size found
// across terraform/, cloudformation/, iac/, or arm/). A set but
// unparseable value is a startup error rather than a silently-ignored
// typo: flipping this flag is a deploy-time contract with that Terraform
// setting, and getting it wrong risks silently dropping failed purchase
// messages instead of redelivering them (see handleLambdaSQSEvent).
func loadReportSQSBatchItemFailures() (bool, error) {
	raw := os.Getenv("SQS_REPORT_BATCH_ITEM_FAILURES")
	if raw == "" {
		return false, nil
	}
	val, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid SQS_REPORT_BATCH_ITEM_FAILURES %q: must be a boolean: %w", raw, err)
	}
	return val, nil
}

// handleLambdaScheduledEvent processes scheduled/cron events.
func (app *Application) handleLambdaScheduledEvent(ctx context.Context, rawEvent json.RawMessage) (any, error) {
	taskType, params, err := ParseScheduledEvent(rawEvent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse scheduled event: %w", err)
	}

	return app.HandleScheduledTask(ctx, taskType, params)
}
