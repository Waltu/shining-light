package ocpp

import (
	"encoding/json"
	"fmt"
)

// MessageType represents the OCPP message type indicator.
type MessageType int

const (
	MessageTypeCall       MessageType = 2 // Request from client to server
	MessageTypeCallResult MessageType = 3 // Response from server to client
	MessageTypeCallError  MessageType = 4 // Error response from server to client
)

// GenericMessage represents the structure of any OCPP message.
// We use []interface{} initially as the JSON is an array with mixed types.
// Index 0: MessageTypeId (int)
// Index 1: MessageId (string)
// Index 2: Action (string)
// Index 3: Payload (json.RawMessage or specific struct)
type GenericMessage []interface{}

// Specific payload structs (add more as needed)

// BootNotificationReq contains the payload for a BootNotification request.
// Section 4.2 BootNotification Feature spec
type BootNotificationReq struct {
	ChargePointVendor string `json:"chargePointVendor"`
	ChargePointModel  string `json:"chargePointModel"`
	// Optional fields omitted for brevity
}

// BootNotificationConf contains the payload for a BootNotification response.
// Section 4.2 BootNotification Feature spec
type BootNotificationConf struct {
	CurrentTime string `json:"currentTime"` // Required. Server time in UTC (ISO 8601)
	Interval    int    `json:"interval"`    // Required. Heartbeat interval in seconds.
	Status      string `json:"status"`      // Required. Registration status (Accepted, Pending, Rejected)
}

// RegistrationStatus represents the possible statuses in BootNotificationConf.
const (
	RegistrationStatusAccepted = "Accepted"
	RegistrationStatusPending  = "Pending"
	RegistrationStatusRejected = "Rejected"
)

// HeartbeatConf contains the payload for a Heartbeat response.
// Section 4.4 Heartbeat Feature spec
type HeartbeatConf struct {
	CurrentTime string `json:"currentTime"` // Required. Server time in UTC (ISO 8601)
}

// StatusNotificationConf contains the payload for a StatusNotification response.
// Section 4.10 StatusNotification Feature spec
type StatusNotificationConf struct{} // Empty object

// IdTagInfo contains information about the authorization status of an IdTag.
// Appendix B. Standard Fields
type IdTagInfo struct {
	Status string `json:"status"` // Required. Status (Accepted, Blocked, Expired, Invalid, ConcurrentTx)
	// ExpiryDate string `json:"expiryDate,omitempty"` // Optional
	// ParentIdTag string `json:"parentIdTag,omitempty"` // Optional
}

// AuthorizationStatus represents the possible statuses in IdTagInfo.
const (
	AuthorizationStatusAccepted     = "Accepted"
	AuthorizationStatusBlocked      = "Blocked"
	AuthorizationStatusExpired      = "Expired"
	AuthorizationStatusInvalid      = "Invalid"
	AuthorizationStatusConcurrentTx = "ConcurrentTx"
)

// AuthorizeConf contains the payload for an Authorize response.
// Section 4.1 Authorize Feature spec
type AuthorizeConf struct {
	IdTagInfo IdTagInfo `json:"idTagInfo"` // Required.
}

// StartTransactionConf contains the payload for a StartTransaction response.
// Section 4.9 StartTransaction Feature spec
type StartTransactionConf struct {
	IdTagInfo     IdTagInfo `json:"idTagInfo"`     // Required.
	TransactionId int       `json:"transactionId"` // Required.
}

// StopTransactionConf contains the payload for a StopTransaction response.
// Section 4.11 StopTransaction Feature spec
type StopTransactionConf struct {
	IdTagInfo IdTagInfo `json:"idTagInfo,omitempty"` // Optional.
}

// MeterValuesConf contains the payload for a MeterValues response.
// Section 4.7 MeterValues Feature spec
type MeterValuesConf struct{} // Empty object

// StartTransactionReq contains the payload for a StartTransaction request.
// Section 4.9 StartTransaction Feature spec
type StartTransactionReq struct {
	ConnectorId   int    `json:"connectorId"`             // Required.
	IdTag         string `json:"idTag"`                   // Required.
	MeterStart    int    `json:"meterStart"`              // Required. Meter value in Wh.
	Timestamp     string `json:"timestamp"`               // Required. ISO 8601 UTC timestamp.
	ReservationId int    `json:"reservationId,omitempty"` // Optional.
}

// StopTransactionReq contains the payload for a StopTransaction request.
// Section 4.11 StopTransaction Feature spec
type StopTransactionReq struct {
	MeterStop     int    `json:"meterStop"`        // Required. Meter value in Wh.
	Timestamp     string `json:"timestamp"`        // Required. ISO 8601 UTC timestamp.
	TransactionId int    `json:"transactionId"`    // Required.
	Reason        string `json:"reason,omitempty"` // Optional. E.g., EmergencyStop, EVDisconnected, HardReset, etc.
	// IdTag         string `json:"idTag,omitempty"` // Optional.
	// TransactionData []MeterValue `json:"transactionData,omitempty"` // Optional. MeterValue array.
}

// DataTransferReq contains the payload for a DataTransfer request.
// Section 4.3 DataTransfer Feature spec
type DataTransferReq struct {
	VendorId  string `json:"vendorId"`            // Required. ID of the vendor.
	MessageId string `json:"messageId,omitempty"` // Optional. Message identifier.
	Data      string `json:"data,omitempty"`      // Optional. Data payload.
}

// DataTransferConf contains the payload for a DataTransfer response.
// Section 4.3 DataTransfer Feature spec
type DataTransferConf struct {
	Status string `json:"status"`         // Required. Status (Accepted, Rejected, UnknownMessageId, UnknownVendorId)
	Data   string `json:"data,omitempty"` // Optional. Data payload.
}

// DataTransferStatus represents the possible statuses in DataTransferConf.
const (
	DataTransferStatusAccepted         = "Accepted"
	DataTransferStatusRejected         = "Rejected"
	DataTransferStatusUnknownMessageId = "UnknownMessageId"
	DataTransferStatusUnknownVendorId  = "UnknownVendorId"
)

// Helper function to parse the generic message structure
func ParseGenericMessage(data []byte) (MessageType, string, string, json.RawMessage, error) {
	var msg GenericMessage // GenericMessage is []interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return 0, "", "", nil, fmt.Errorf("failed to unmarshal OCPP message array: %w", err)
	}

	// Validate message structure
	if len(msg) != 4 {
		return 0, "", "", nil, fmt.Errorf("invalid OCPP message format: expected 4 elements, got %d", len(msg))
	}

	// Index 0: MessageTypeId (expecting number)
	msgTypeFloat, ok := msg[0].(float64) // JSON numbers decode to float64 in interface{}
	if !ok {
		return 0, "", "", nil, fmt.Errorf("invalid MessageTypeId type: expected number, got %T", msg[0])
	}
	msgType := MessageType(msgTypeFloat)
	if msgType != MessageTypeCall && msgType != MessageTypeCallResult && msgType != MessageTypeCallError {
		// Note: Client might send other types, but this listener primarily expects Call (2)
		// We might want to be stricter or handle other types later.
		// For now, just check it's a valid known type.
	}

	// Index 1: MessageId (expecting string)
	msgID, ok := msg[1].(string)
	if !ok {
		return 0, "", "", nil, fmt.Errorf("invalid MessageId type: expected string, got %T", msg[1])
	}

	// Index 2: Action (expecting string for Call type)
	// For CallResult/CallError, this index holds the payload, and index 3 doesn't exist or is different.
	// This function currently assumes Call format [2, id, action, payload].
	// Handling CallResult [3, id, payload] and CallError [4, id, code, desc, details] would require modification.
	var action string
	var payloadInterface interface{}

	if msgType == MessageTypeCall {
		actionStr, ok := msg[2].(string)
		if !ok {
			return 0, "", "", nil, fmt.Errorf("invalid Action type for Call message: expected string, got %T", msg[2])
		}
		action = actionStr
		payloadInterface = msg[3]
	} else {
		// For CallResult or CallError, treat element 2 as the payload and Action as empty/irrelevant here
		action = "" // Or determine action based on context if needed elsewhere
		payloadInterface = msg[2]
		// We should check len(msg) is 3 for CallResult/CallError here if being strict
	}

	// Index 3/2: Payload (expecting JSON object/value)
	// Remarshal the interface{} back to JSON bytes to get json.RawMessage
	payloadBytes, err := json.Marshal(payloadInterface)
	if err != nil {
		return 0, "", "", nil, fmt.Errorf("failed to remarshal payload: %w", err)
	}
	payload := json.RawMessage(payloadBytes)

	return msgType, msgID, action, payload, nil
}
