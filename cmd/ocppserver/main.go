package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time" // Needed for Heartbeat/BootNotification response

	"github.com/go-redis/redis/v8"
	"github.com/waltu/shining-light/pkg/ocpp"
	commanderPb "github.com/waltu/shining-light/proto/chargercommander" // Added commander import
	forwarderPb "github.com/waltu/shining-light/proto/ocppforwarder"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure" // Added insecure
)

// Redis client (global or passed through context/DI)
var rdb *redis.Client

// gRPC client (to listener's commander service)
var (
	commanderConn   *grpc.ClientConn
	commanderClient commanderPb.ChargerCommanderClient
	commanderTarget = "localhost:50052" // Listener's commander port
)

// server is used to implement ocppforwarder.OcppForwarderServer.
type server struct {
	forwarderPb.UnimplementedOcppForwarderServer
	// No need to store rdb here if it's global, but could be passed for testing
}

// ForwardMessage implements ocppforwarder.OcppForwarderServer
func (s *server) ForwardMessage(ctx context.Context, in *forwarderPb.OcppMessage) (*forwarderPb.ForwardResponse, error) {
	// Use the MessageId field from the incoming gRPC message
	log.Printf("Received raw message from ChargePointId: %s (MsgID: %s)", in.ChargePointId, in.MessageId)

	// Parse the generic message
	msgType, _, action, payload, err := ocpp.ParseGenericMessage(in.RawMessage) // Original MsgID not needed here, it's in 'in.MessageId'
	if err != nil {
		log.Printf("Error parsing OCPP message for %s: %v", in.ChargePointId, err)
		// Use correct field names from regenerated proto
		return &forwarderPb.ForwardResponse{ProcessedSuccessfully: false, ResponseAction: forwarderPb.ResponseAction_DO_NOTHING, ErrorMessage: "Failed to parse OCPP message"}, nil
	}

	log.Printf("Parsed Action: %s, Type: %d for %s", action, msgType, in.ChargePointId)

	chargerKey := fmt.Sprintf("charger:%s", in.ChargePointId)
	var responsePayloadBytes []byte
	responseAction := forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
	processedSuccessfully := true
	errorMessage := ""

	if msgType == ocpp.MessageTypeCall {
		var responsePayloadStruct interface{}
		responseAction = forwarderPb.ResponseAction_SEND_RESPONSE // Use correct enum constant

		switch action {
		case "BootNotification":
			var req ocpp.BootNotificationReq
			if err := json.Unmarshal(payload, &req); err != nil {
				log.Printf("Error unmarshalling BootNotification for %s: %v", in.ChargePointId, err)
				processedSuccessfully = false
				errorMessage = "Failed to unmarshal BootNotification payload"
				responseAction = forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
			} else {
				log.Printf("Handling BootNotification for %s: Vendor=%s, Model=%s", in.ChargePointId, req.ChargePointVendor, req.ChargePointModel)
				rdsErr := rdb.HSet(ctx, chargerKey, "vendor", req.ChargePointVendor, "model", req.ChargePointModel, "status", "Available").Err()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to update Redis for %s (BootNotification): %v", chargerKey, rdsErr)
					processedSuccessfully = false // Mark as failed, but maybe still Accept?
					// Let's still Accept the boot for now
				}
				// Prepare response
				responsePayloadStruct = ocpp.BootNotificationConf{
					CurrentTime: time.Now().UTC().Format(time.RFC3339Nano),
					Interval:    60, // TODO: Make configurable
					Status:      ocpp.RegistrationStatusAccepted,
				}
			}
		case "Heartbeat":
			log.Printf("Handling Heartbeat for %s", in.ChargePointId)
			// TODO: Update last heartbeat time in Redis?
			// Prepare response
			responsePayloadStruct = ocpp.HeartbeatConf{
				CurrentTime: time.Now().UTC().Format(time.RFC3339Nano),
			}
		case "StatusNotification":
			// Process the notification (update Redis)
			var statusData map[string]interface{}
			status := "Unknown"
			if err := json.Unmarshal(payload, &statusData); err == nil {
				if st, ok := statusData["status"].(string); ok {
					status = st
				}
			}
			log.Printf("Handling StatusNotification for %s: Status=%s", in.ChargePointId, status)
			rdsErr := rdb.HSet(ctx, chargerKey, "status", status).Err()
			if rdsErr != nil {
				log.Printf("ERROR: Failed to update Redis for %s (StatusNotification): %v", chargerKey, rdsErr)
				processedSuccessfully = false
			}
			// Prepare response (empty object)
			responsePayloadStruct = ocpp.StatusNotificationConf{}

		case "Authorize":
			log.Printf("Handling Authorize for %s...", in.ChargePointId)
			// TODO: Implement real auth check
			responsePayloadStruct = ocpp.AuthorizeConf{IdTagInfo: ocpp.IdTagInfo{Status: ocpp.AuthorizationStatusAccepted}}
		case "StartTransaction":
			var req ocpp.StartTransactionReq
			if err := json.Unmarshal(payload, &req); err != nil {
				log.Printf("Error unmarshalling StartTransaction for %s: %v", in.ChargePointId, err)
				processedSuccessfully = false
				errorMessage = "Failed to unmarshal StartTransaction payload"
				responseAction = forwarderPb.ResponseAction_DO_NOTHING
			} else {
				// --- Generate Transaction ID using Redis HINCRBY ---
				// chargerKey should already be defined from the start of ForwardMessage
				newTransactionID64, rdsErr := rdb.HIncrBy(ctx, chargerKey, "lastTransactionId", 1).Result()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to increment transaction ID in Redis for %s: %v", chargerKey, rdsErr)
					// Fail the operation as we couldn't get a valid transaction ID
					processedSuccessfully = false
					errorMessage = "Internal server error (Redis transaction ID generation failed)"
					responseAction = forwarderPb.ResponseAction_DO_NOTHING
					// Exit this case block since we failed critical step
					break
				}
				newTransactionID := int(newTransactionID64) // Convert from int64 returned by HIncrBy
				// --- End Transaction ID Generation ---

				log.Printf("Handling StartTransaction for %s: Tag=%s, Meter=%d. Assigning TransactionID: %d", in.ChargePointId, req.IdTag, req.MeterStart, newTransactionID)
				// Redis logic using the generated newTransactionID ...
				transactionKey := fmt.Sprintf("transaction:%d", newTransactionID)
				rdsErr = rdb.HSet(ctx, transactionKey, "chargerId", in.ChargePointId, "idTag", req.IdTag, "meterStart", req.MeterStart, "startTimestamp", req.Timestamp, "status", "InProgress", "connectorId", req.ConnectorId).Err()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to create Redis transaction %s: %v", transactionKey, rdsErr)
					processedSuccessfully = false
				}
				rdsErr = rdb.HSet(ctx, chargerKey, "status", "Charging", "currentTransaction", newTransactionID).Err()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to update charger status for %s: %v", chargerKey, rdsErr)
					processedSuccessfully = false
				}

				// Prepare response using the generated newTransactionID
				responsePayloadStruct = ocpp.StartTransactionConf{
					IdTagInfo:     ocpp.IdTagInfo{Status: ocpp.AuthorizationStatusAccepted},
					TransactionId: newTransactionID,
				}
			}
		case "StopTransaction":
			var req ocpp.StopTransactionReq
			if err := json.Unmarshal(payload, &req); err != nil {
				log.Printf("Error unmarshalling StopTransaction for %s: %v", in.ChargePointId, err)
				processedSuccessfully = false
				errorMessage = "Failed to unmarshal StopTransaction payload"
				responseAction = forwarderPb.ResponseAction_DO_NOTHING
			} else {
				log.Printf("Handling StopTransaction for %s: ID=%d, Meter=%d, Reason=%s", in.ChargePointId, req.TransactionId, req.MeterStop, req.Reason)
				// Redis logic using req ...
				transactionKeyToStop := fmt.Sprintf("transaction:%d", req.TransactionId)
				rdsErr := rdb.HSet(ctx, transactionKeyToStop, "meterStop", req.MeterStop, "stopTimestamp", req.Timestamp, "stopReason", req.Reason, "status", "Completed", "updatedAt", time.Now().UTC().Format(time.RFC3339Nano)).Err()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to update Redis transaction %s: %v", transactionKeyToStop, rdsErr)
					processedSuccessfully = false
				}
				rdsErr = rdb.HSet(ctx, chargerKey, "status", "Available", "currentTransaction", "", "updatedAt", time.Now().UTC().Format(time.RFC3339Nano)).Err()
				if rdsErr != nil {
					log.Printf("ERROR: Failed to update charger status for %s: %v", chargerKey, rdsErr)
					processedSuccessfully = false
				}

				// Prepare response
				responsePayloadStruct = ocpp.StopTransactionConf{IdTagInfo: ocpp.IdTagInfo{Status: ocpp.AuthorizationStatusAccepted}}
			}
		case "MeterValues":
			log.Printf("Handling MeterValues for %s...", in.ChargePointId)
			// TODO: Process meter values if needed
			responsePayloadStruct = ocpp.MeterValuesConf{}
		case "DataTransfer":
			var req ocpp.DataTransferReq
			if err := json.Unmarshal(payload, &req); err != nil {
				log.Printf("Error unmarshalling DataTransfer for %s: %v", in.ChargePointId, err)
				processedSuccessfully = false
				errorMessage = "Failed to unmarshal DataTransfer payload"
				responseAction = forwarderPb.ResponseAction_DO_NOTHING
			} else {
				log.Printf("Handling DataTransfer for %s: VendorId=%s...", in.ChargePointId, req.VendorId)
				// TODO: Implement vendor logic
				responsePayloadStruct = ocpp.DataTransferConf{Status: ocpp.DataTransferStatusAccepted}
			}

		default:
			log.Printf("Unknown action '%s' for %s", action, in.ChargePointId)
			processedSuccessfully = false
			responseAction = forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
			errorMessage = "Unknown action"
		}

		// Marshal the response payload if one was prepared
		if responseAction == forwarderPb.ResponseAction_SEND_RESPONSE && responsePayloadStruct != nil {
			payloadBytes, marshalErr := json.Marshal(responsePayloadStruct)
			if marshalErr != nil {
				log.Printf("ERROR: Failed to marshal response payload for %s (%s): %v", in.ChargePointId, action, marshalErr)
				processedSuccessfully = false
				errorMessage = "Failed to create response payload"
				responseAction = forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
			} else {
				responsePayloadBytes = payloadBytes
			}
		} else if responseAction == forwarderPb.ResponseAction_SEND_RESPONSE {
			// If action was SEND_RESPONSE but payload struct is nil (e.g., due to unmarshal error above)
			responseAction = forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
		}
	} else {
		// Message was not a Call (e.g., CallResult, CallError from charger?)
		log.Printf("Received non-Call message type %d from %s. Ignoring.", msgType, in.ChargePointId)
		responseAction = forwarderPb.ResponseAction_DO_NOTHING // Use correct enum constant
	}

	// Construct and return the gRPC response using correct field names
	return &forwarderPb.ForwardResponse{
		ProcessedSuccessfully: processedSuccessfully,
		ResponseAction:        responseAction,
		ResponsePayload:       responsePayloadBytes,
		ErrorMessage:          errorMessage,
	}, nil
}

// NotifyCommandResult implements ocppforwarder.OcppForwarderServer
func (s *server) NotifyCommandResult(ctx context.Context, in *forwarderPb.CommandResult) (*forwarderPb.NotificationResponse, error) {
	log.Printf("NOTIFY: Received command result for Charger: %s, OriginalMsgID: %s", in.ChargePointId, in.OriginalMessageId)

	if in.Success {
		log.Printf("NOTIFY: Command successful. Payload: %s", string(in.ResponsePayload))
		// TODO: Process successful response payload if needed
	} else {
		log.Printf("NOTIFY: Command failed. ErrorCode: %s, Description: %s, Details: %s",
			in.ErrorCode, in.ErrorDescription, string(in.ErrorDetails))
		// TODO: Handle command failure
	}

	// Acknowledge receipt of the notification
	return &forwarderPb.NotificationResponse{Acknowledged: true}, nil
}

func setupCommanderClient() error {
	var err error
	// TODO: Add credentials
	commanderConn, err = grpc.NewClient(commanderTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("COMMANDER CLIENT: did not connect: %v", err)
		return err
	}
	commanderClient = commanderPb.NewChargerCommanderClient(commanderConn)
	log.Printf("COMMANDER CLIENT: Connected to listener commander at %s", commanderTarget)
	return nil
}

func closeCommanderClient() {
	if commanderConn != nil {
		commanderConn.Close()
	}
}

// Example function showing how to send a command (NOT CALLED YET)
func exampleSendCommand(chargePointId string, action string, payload interface{}) {
	if commanderClient == nil {
		log.Println("ERROR: Commander client not initialized.")
		return
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: Failed to marshal command payload: %v", err)
		return
	}

	req := &commanderPb.SendCommandRequest{
		ChargePointId:  chargePointId,
		MessageId:      fmt.Sprintf("server-msg-%d", time.Now().UnixNano()), // Generate unique ID
		Action:         action,
		RequestPayload: payloadBytes,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	resp, err := commanderClient.SendCommand(ctx, req)
	if err != nil {
		log.Printf("ERROR: SendCommand RPC failed for %s: %v", chargePointId, err)
	} else if !resp.Success {
		log.Printf("WARN: SendCommand failed on listener for %s: %s", chargePointId, resp.ErrorMessage)
	} else {
		log.Printf("INFO: Command '%s' sent successfully to listener for %s", action, chargePointId)
	}
}

func main() {
	// Initialize Redis Client
	rdb = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379", // Make configurable
		Password: "",               // no password set
		DB:       0,                // use default DB
	})

	// Ping Redis to check connection
	ctx := context.Background()
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("Could not connect to Redis: %v", err)
	}
	log.Println("Connected to Redis")

	// Initialize gRPC client (to listener's commander service)
	if err := setupCommanderClient(); err != nil {
		log.Printf("Failed to setup Commander client: %v. Cannot send commands.", err)
	}
	defer closeCommanderClient()

	// Start gRPC server (for receiving forwarded messages AND command results from listener)
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	// Register the forwarder server implementation (which now includes NotifyCommandResult)
	forwarderPb.RegisterOcppForwarderServer(s, &server{})
	log.Printf("FORWARDER SERVER: gRPC server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("FORWARDER SERVER: failed to serve: %v", err)
	}
}
