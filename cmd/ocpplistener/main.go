package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/waltu/shining-light/pkg/ocpp"
	commanderPb "github.com/waltu/shining-light/proto/chargercommander" // Alias for commander proto
	forwarderPb "github.com/waltu/shining-light/proto/ocppforwarder"    // Alias for forwarder proto
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all connections for now
		return true
	},
}

// Map to store active WebSocket connections -> chargePointId: *websocket.Conn
// Needs to be concurrent-safe
var activeConnections sync.Map

// gRPC client setup (to ocppserver)
var (
	grpcConn        *grpc.ClientConn
	forwarderClient forwarderPb.OcppForwarderClient // Use alias
	grpcTarget      = "localhost:50051"
)

func setupGrpcClient() error {
	var err error
	// Set up a connection to the server.
	// TODO: Add proper credentials and error handling/retry logic
	grpcConn, err = grpc.NewClient(grpcTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("did not connect: %v", err)
		return err
	}
	forwarderClient = forwarderPb.NewOcppForwarderClient(grpcConn)
	log.Printf("Connected to gRPC forwarder at %s", grpcTarget)
	return nil
}

func closeGrpcClient() {
	if grpcConn != nil {
		grpcConn.Close()
	}
}

// --- Charger Commander gRPC Server Implementation ---

// commanderServer implements chargercommander.ChargerCommanderServer
type commanderServer struct {
	commanderPb.UnimplementedChargerCommanderServer
}

// SendCommand sends a command down the WebSocket connection to a specific charger
func (s *commanderServer) SendCommand(ctx context.Context, req *commanderPb.SendCommandRequest) (*commanderPb.SendCommandResponse, error) {
	chargePointId := req.ChargePointId
	log.Printf("COMMANDER: Received SendCommand request for %s (Action: %s, MsgID: %s)", chargePointId, req.Action, req.MessageId)

	connVal, ok := activeConnections.Load(chargePointId)
	if !ok {
		log.Printf("COMMANDER: Charger %s not connected.", chargePointId)
		return &commanderPb.SendCommandResponse{Success: false, ErrorMessage: "Charger not connected"}, nil
	}

	conn, ok := connVal.(*websocket.Conn)
	if !ok {
		// Should not happen if we store correctly
		log.Printf("COMMANDER: Internal error - connection map stored invalid type for %s", chargePointId)
		activeConnections.Delete(chargePointId) // Clean up bad entry
		return &commanderPb.SendCommandResponse{Success: false, ErrorMessage: "Internal server error (invalid connection type)"}, nil
	}

	// Unmarshal the request payload bytes into a Go type for WriteJSON
	var requestPayloadData interface{}
	if err := json.Unmarshal(req.RequestPayload, &requestPayloadData); err != nil {
		log.Printf("COMMANDER: Failed to unmarshal request payload for %s: %v", chargePointId, err)
		return &commanderPb.SendCommandResponse{Success: false, ErrorMessage: "Invalid request payload JSON"}, nil
	}

	// Construct OCPP Call message: [2, messageId, action, payload]
	callMsg := []interface{}{ocpp.MessageTypeCall, req.MessageId, req.Action, requestPayloadData}

	// Send the message over the WebSocket
	// TODO: Need mutex for WriteJSON per connection? websocket package says concurrent writes need locking.
	err := conn.WriteJSON(callMsg)
	if err != nil {
		log.Printf("COMMANDER: Error sending command to %s: %v", chargePointId, err)
		// Consider removing connection from map if write fails?
		return &commanderPb.SendCommandResponse{Success: false, ErrorMessage: fmt.Sprintf("WebSocket write error: %v", err)}, nil
	}

	log.Printf("COMMANDER: Successfully sent command %s to %s", req.Action, chargePointId)
	return &commanderPb.SendCommandResponse{Success: true}, nil
}

// Function to start the commander gRPC server
func startCommanderServer(port string) {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("COMMANDER: failed to listen on %s: %v", port, err)
	}
	s := grpc.NewServer()
	commanderPb.RegisterChargerCommanderServer(s, &commanderServer{})
	log.Printf("COMMANDER: gRPC server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("COMMANDER: failed to serve: %v", err)
	}
}

// --- End Commander Server ---

func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Extract chargePointId from URL path
	vars := mux.Vars(r)
	chargePointId, ok := vars["chargePointId"]
	if !ok || chargePointId == "" {
		// Reject connection if ID is missing
		log.Println("Rejecting connection: chargePointId missing from URL")
		http.Error(w, "Missing chargePointId in path", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade failed for %s: %v", chargePointId, err)
		return
	}
	// Ensure connection is closed when handler exits
	defer conn.Close()

	// Store the active connection
	log.Printf("Client connected: %s (ID: %s)", conn.RemoteAddr(), chargePointId)
	activeConnections.Store(chargePointId, conn)
	// Ensure connection is removed when handler exits
	defer func() {
		log.Printf("Removing connection for %s", chargePointId)
		activeConnections.Delete(chargePointId)
	}()

	// --- Message processing loop ---
	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading message from %s: %v", chargePointId, err)
			} else {
				log.Printf("Client %s disconnected gracefully.", chargePointId)
			}
			break
		}

		// We need the original message ID for CallResult/CallError handling
		msgType, originalMsgID, action, payloadBytes, err := ocpp.ParseGenericMessage(p)
		if err != nil {
			log.Printf("Error parsing OCPP message from %s: %v\n", chargePointId, err)
			continue // Skip to the next message
		}

		if msgType == ocpp.MessageTypeCall {
			// --- Handle CALL from Charger ---
			log.Printf("Received CALL Action: %s, MessageID: %s, Type: %d from %s\n", action, originalMsgID, msgType, chargePointId)

			// Forward the message using gRPC and handle response from server
			if forwarderClient != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)

				forwardReq := &forwarderPb.OcppMessage{
					ChargePointId: chargePointId,
					MessageId:     originalMsgID, // Send original MessageID to server
					RawMessage:    p,
				}

				grpcResp, err := forwarderClient.ForwardMessage(ctx, forwardReq)
				cancel()

				if err != nil {
					log.Printf("ERROR: Failed to forward CALL via gRPC for %s: %v\n", chargePointId, err)
				} else if !grpcResp.ProcessedSuccessfully {
					log.Printf("WARN: gRPC forwarder indicated processing failure for CALL %s: %s\n", chargePointId, grpcResp.ErrorMessage)
				} else {
					if grpcResp.ResponseAction == forwarderPb.ResponseAction_SEND_RESPONSE {
						if len(grpcResp.ResponsePayload) > 0 {
							var responsePayloadData interface{}
							if umErr := json.Unmarshal(grpcResp.ResponsePayload, &responsePayloadData); umErr != nil {
								log.Printf("ERROR: Failed to unmarshal response payload from gRPC server for %s: %v", chargePointId, umErr)
							} else {
								responseMsg := []interface{}{ocpp.MessageTypeCallResult, originalMsgID, responsePayloadData}
								log.Printf("Sending CallResult response to %s (ID: %s) based on server response", chargePointId, originalMsgID)
								werr := conn.WriteJSON(responseMsg)
								if werr != nil {
									log.Printf("ERROR sending CallResult to %s: %v", chargePointId, werr)
								}
							}
						} else {
							log.Printf("WARN: Server requested SEND_RESPONSE for %s but payload was empty.", chargePointId)
						}
					} else {
						log.Printf("CALL Message forwarded for %s, no OCPP response required by server.", chargePointId)
					}
				}
			} else {
				log.Println("WARN: gRPC client to server not initialized, cannot forward CALL message.")
			}
			// --- End Handle CALL ---

		} else if msgType == ocpp.MessageTypeCallResult {
			// --- Handle CALL RESULT from Charger ---
			log.Printf("Received CALL RESULT MessageID: %s, Type: %d from %s\n", originalMsgID, msgType, chargePointId)

			// Notify the server about the result
			if forwarderClient != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
				notifyReq := &forwarderPb.CommandResult{
					ChargePointId:     chargePointId,
					OriginalMessageId: originalMsgID,
					Success:           true,
					ResponsePayload:   payloadBytes, // Payload of the CallResult
				}
				_, err := forwarderClient.NotifyCommandResult(ctx, notifyReq)
				cancel()
				if err != nil {
					log.Printf("ERROR: Failed to notify server of CallResult for %s: %v", chargePointId, err)
				}
			} else {
				log.Println("WARN: gRPC client to server not initialized, cannot notify server of CallResult.")
			}
			// --- End Handle CALL RESULT ---

		} else if msgType == ocpp.MessageTypeCallError {
			// --- Handle CALL ERROR from Charger ---
			log.Printf("Received CALL ERROR MessageID: %s, Type: %d from %s\n", originalMsgID, msgType, chargePointId)

			// TODO: Parse the CallError payload [errorCode, errorDescription, errorDetails{}] properly
			// For now, send the raw payload as errorDetails
			errorCode := "UnknownError"
			errorDesc := ""
			errorDetails := payloadBytes

			// Notify the server about the error
			if forwarderClient != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
				notifyReq := &forwarderPb.CommandResult{
					ChargePointId:     chargePointId,
					OriginalMessageId: originalMsgID,
					Success:           false,
					ErrorCode:         errorCode,
					ErrorDescription:  errorDesc,
					ErrorDetails:      errorDetails,
				}
				_, err := forwarderClient.NotifyCommandResult(ctx, notifyReq)
				cancel()
				if err != nil {
					log.Printf("ERROR: Failed to notify server of CallError for %s: %v", chargePointId, err)
				}
			} else {
				log.Println("WARN: gRPC client to server not initialized, cannot notify server of CallError.")
			}
			// --- End Handle CALL ERROR ---
		} else {
			log.Printf("WARN: Received unhandled message type %d from %s", msgType, chargePointId)
		}

	}
	// --- End Message processing loop ---

	log.Printf("Client %s processing loop finished.", chargePointId)
}

func main() {
	// Setup gRPC client connection (to ocppserver)
	if err := setupGrpcClient(); err != nil {
		log.Printf("Failed to setup gRPC client: %v. Proceeding without forwarding.", err)
	}
	defer closeGrpcClient()

	// Start the gRPC *server* for receiving commands (run in background)
	go startCommanderServer(":50052") // Use different port

	// Setup HTTP router for incoming WebSocket connections
	r := mux.NewRouter()
	r.HandleFunc("/{chargePointId}", wsHandler)

	log.Println("Starting OCPP listener on :8080, expecting path /{chargePointId}")
	err := http.ListenAndServe(":8080", r)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
