package control

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	controlpb "github.com/chuanqq/remote-cli/internal/control/proto"
)

// Control protocol version. A master accepts only the same major and allows
// higher minors (design 03 §7).
const (
	ProtocolMajor = 1
	ProtocolMinor = 0

	// MaxMessageBytes bounds a single control frame. Larger tool results must
	// be chunked (design 03 §11); the Phase 2 invoke path stays well below it.
	MaxMessageBytes = 4 << 20
)

// frame builds an envelope with the shared protocol/version metadata.
func frame(kind string) *controlpb.Frame {
	return &controlpb.Frame{
		ProtocolMajor: ProtocolMajor,
		ProtocolMinor: ProtocolMinor,
		MessageType:   kind,
		SentAt:        timestamppb.Now(),
	}
}

func registerAckFrame(nodeID, instanceID string, epoch uint64, status string,
	heartbeat, lease uint32, masterVersion, policyRevision string) *controlpb.Frame {
	f := frame("register_ack")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.Payload = &controlpb.Frame_RegisterAck{RegisterAck: &controlpb.RegisterAck{
		ConnectionEpoch:      epoch,
		HeartbeatIntervalSec: heartbeat,
		LeaseTtlSec:          lease,
		MasterVersion:        masterVersion,
		PolicyRevision:       policyRevision,
		Status:               status,
	}}
	return f
}

func rejectFrame(nodeID, code, message string, retryable bool) *controlpb.Frame {
	f := frame("register_reject")
	f.NodeId = nodeID
	f.Payload = &controlpb.Frame_RegisterReject{RegisterReject: &controlpb.RegisterReject{
		Code:      code,
		Message:   message,
		Retryable: retryable,
	}}
	return f
}

func errorFrame(nodeID, code, message string, retryable bool) *controlpb.Frame {
	f := frame("error")
	f.NodeId = nodeID
	f.Payload = &controlpb.Frame_Error{Error: &controlpb.ErrorMessage{
		Code:      code,
		Message:   message,
		Retryable: retryable,
		NodeId:    nodeID,
	}}
	return f
}

func heartbeatAckFrame(sequence uint64) *controlpb.Frame {
	f := frame("heartbeat_ack")
	f.Payload = &controlpb.Frame_HeartbeatAck{HeartbeatAck: &controlpb.HeartbeatAck{Sequence: sequence}}
	return f
}

func heartbeatFrame(nodeID, instanceID string, epoch, sequence uint64, load LoadInfo) *controlpb.Frame {
	f := frame("heartbeat")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.Sequence = sequence
	f.Payload = &controlpb.Frame_Heartbeat{Heartbeat: &controlpb.Heartbeat{
		Sequence: sequence,
		Load: &controlpb.Load{
			RunningExecutions: load.RunningExecutions,
			ActiveScans:       load.ActiveScans,
			RunningJobs:       load.RunningJobs,
		},
	}}
	return f
}

func registerFrame(nodeID, instanceID string, req *controlpb.RegisterRequest) *controlpb.Frame {
	f := frame("register")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.Payload = &controlpb.Frame_Register{Register: req}
	return f
}

func acceptedFrame(nodeID, instanceID string, epoch uint64, operationID, localTaskID, taskKind string) *controlpb.Frame {
	f := frame("accepted")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.OperationId = operationID
	f.Payload = &controlpb.Frame_Accepted{Accepted: &controlpb.Accepted{
		OperationId: operationID,
		LocalTaskId: localTaskID,
		TaskKind:    taskKind,
		AcceptedAt:  timestamppb.Now(),
	}}
	return f
}

func resultFrame(nodeID, instanceID string, epoch uint64, result *controlpb.Result) *controlpb.Frame {
	f := frame("result")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.OperationId = result.OperationId
	f.Sequence = result.Sequence
	f.Payload = &controlpb.Frame_Result{Result: result}
	return f
}

func queryResultFrame(nodeID, instanceID string, epoch uint64, qr *controlpb.QueryResult) *controlpb.Frame {
	f := frame("query_result")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.OperationId = qr.OperationId
	f.Payload = &controlpb.Frame_QueryResult{QueryResult: qr}
	return f
}

func cancelAckFrame(nodeID, instanceID string, epoch uint64, operationID, state string) *controlpb.Frame {
	f := frame("cancel_ack")
	f.NodeId = nodeID
	f.InstanceId = instanceID
	f.ConnectionEpoch = epoch
	f.OperationId = operationID
	f.Payload = &controlpb.Frame_CancelAck{CancelAck: &controlpb.CancelAck{
		OperationId: operationID,
		State:       state,
	}}
	return f
}

func closeFrame(nodeID, reason string) *controlpb.Frame {
	f := frame("close")
	f.NodeId = nodeID
	f.Payload = &controlpb.Frame_Close{Close: &controlpb.Close{Reason: reason}}
	return f
}
