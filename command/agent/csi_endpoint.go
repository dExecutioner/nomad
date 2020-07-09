package agent

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

func (s *HTTPServer) CSIVolumesRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != "GET" {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	// Type filters volume lists to a specific type. When support for non-CSI volumes is
	// introduced, we'll need to dispatch here
	query := req.URL.Query()
	qtype, ok := query["type"]
	if !ok {
		return []*structs.CSIVolListStub{}, nil
	}
	if qtype[0] != "csi" {
		return nil, nil
	}

	args := structs.CSIVolumeListRequest{}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	if plugin, ok := query["plugin_id"]; ok {
		args.PluginID = plugin[0]
	}
	if node, ok := query["node_id"]; ok {
		args.NodeID = node[0]
	}

	var out structs.CSIVolumeListResponse
	if err := s.agent.RPC("CSIVolume.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	return out.Volumes, nil
}

// CSIVolumeSpecificRequest dispatches GET and PUT
func (s *HTTPServer) CSIVolumeSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Tokenize the suffix of the path to get the volume id
	reqSuffix := strings.TrimPrefix(req.URL.Path, "/v1/volume/csi/")
	tokens := strings.Split(reqSuffix, "/")
	if len(tokens) > 2 || len(tokens) < 1 {
		return nil, CodedError(404, resourceNotFoundErr)
	}
	id := tokens[0]

	switch req.Method {
	case "GET":
		return s.csiVolumeGet(id, resp, req)
	case "PUT":
		return s.csiVolumePut(id, resp, req)
	case "DELETE":
		return s.csiVolumeDelete(id, resp, req)
	default:
		return nil, CodedError(405, ErrInvalidMethod)
	}
}

func (s *HTTPServer) csiVolumeGet(id string, resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	args := structs.CSIVolumeGetRequest{
		ID: id,
	}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.CSIVolumeGetResponse
	if err := s.agent.RPC("CSIVolume.Get", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Volume == nil {
		return nil, CodedError(404, "volume not found")
	}

	// remove sensitive fields, as our redaction mechanism doesn't
	// help serializing here
	out.Volume.Secrets = nil
	out.Volume.MountOptions = nil
	return out.Volume, nil
}

func (s *HTTPServer) csiVolumePut(id string, resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != "PUT" {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	args0 := structs.CSIVolumeRegisterRequest{}
	if err := decodeBody(req, &args0); err != nil {
		return err, CodedError(400, err.Error())
	}

	args := structs.CSIVolumeRegisterRequest{
		Volumes: args0.Volumes,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CSIVolumeRegisterResponse
	if err := s.agent.RPC("CSIVolume.Register", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return nil, nil
}

func (s *HTTPServer) csiVolumeDelete(id string, resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != "DELETE" {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	raw := req.URL.Query().Get("force")
	var force bool
	if raw != "" {
		var err error
		force, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, CodedError(400, "invalid force value")
		}
	}

	args := structs.CSIVolumeDeregisterRequest{
		VolumeIDs: []string{id},
		Force:     force,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CSIVolumeDeregisterResponse
	if err := s.agent.RPC("CSIVolume.Deregister", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return nil, nil
}

// CSIPluginsRequest lists CSI plugins
func (s *HTTPServer) CSIPluginsRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != "GET" {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	// Type filters plugin lists to a specific type. When support for non-CSI plugins is
	// introduced, we'll need to dispatch here
	query := req.URL.Query()
	qtype, ok := query["type"]
	if !ok {
		return []*structs.CSIPluginListStub{}, nil
	}
	if qtype[0] != "csi" {
		return nil, nil
	}

	args := structs.CSIPluginListRequest{}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.CSIPluginListResponse
	if err := s.agent.RPC("CSIPlugin.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	return out.Plugins, nil
}

// CSIPluginSpecificRequest list the job with CSIInfo
func (s *HTTPServer) CSIPluginSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != "GET" {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	// Tokenize the suffix of the path to get the plugin id
	reqSuffix := strings.TrimPrefix(req.URL.Path, "/v1/plugin/csi/")
	tokens := strings.Split(reqSuffix, "/")
	if len(tokens) > 2 || len(tokens) < 1 {
		return nil, CodedError(404, resourceNotFoundErr)
	}
	id := tokens[0]

	args := structs.CSIPluginGetRequest{ID: id}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.CSIPluginGetResponse
	if err := s.agent.RPC("CSIPlugin.Get", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Plugin == nil {
		return nil, CodedError(404, "plugin not found")
	}

	return out.Plugin, nil
}

// structsCSIPluginToApi converts CSIPlugin, setting Expected the count of known plugin
// instances
func structsCSIPluginToApi(plug *structs.CSIPlugin) *api.CSIPlugin {
	out := &api.CSIPlugin{
		ID:                  plug.ID,
		Provider:            plug.Provider,
		Version:             plug.Version,
		ControllerRequired:  plug.ControllerRequired,
		ControllersHealthy:  plug.ControllersHealthy,
		ControllersExpected: len(plug.Controllers),
		NodesHealthy:        plug.NodesHealthy,
		NodesExpected:       len(plug.Nodes),
		CreateIndex:         plug.CreateIndex,
		ModifyIndex:         plug.ModifyIndex,
	}

	for k, v := range plug.Controllers {
		out.Controllers[k] = structsCSIInfoToApi(v)
	}

	for k, v := range plug.Nodes {
		out.Nodes[k] = structsCSIInfoToApi(v)
	}

	for _, a := range plug.Allocations {
		out.Allocations = append(out.Allocations, structsAllocListStubToApi(a))
	}

	return out
}

// structsCSIInfoToApi converts CSIInfo, part of CSIPlugin
func structsCSIInfoToApi(info *structs.CSIInfo) *api.CSIInfo {
	out := &api.CSIInfo{
		PluginID:                 info.PluginID,
		Healthy:                  info.Healthy,
		HealthDescription:        info.HealthDescription,
		UpdateTime:               info.UpdateTime,
		RequiresControllerPlugin: info.RequiresControllerPlugin,
		RequiresTopologies:       info.RequiresTopologies,
	}

	if info.ControllerInfo != nil {
		out.ControllerInfo = &api.CSIControllerInfo{
			SupportsReadOnlyAttach:           info.ControllerInfo.SupportsReadOnlyAttach,
			SupportsAttachDetach:             info.ControllerInfo.SupportsAttachDetach,
			SupportsListVolumes:              info.ControllerInfo.SupportsListVolumes,
			SupportsListVolumesAttachedNodes: info.ControllerInfo.SupportsListVolumesAttachedNodes,
		}
	}

	if info.NodeInfo != nil {
		out.NodeInfo = &api.CSINodeInfo{
			ID:                      info.NodeInfo.ID,
			MaxVolumes:              info.NodeInfo.MaxVolumes,
			RequiresNodeStageVolume: info.NodeInfo.RequiresNodeStageVolume,
		}

		if info.NodeInfo.AccessibleTopology != nil {
			out.NodeInfo.AccessibleTopology = &api.CSITopology{}
			out.NodeInfo.AccessibleTopology.Segments = info.NodeInfo.AccessibleTopology.Segments
		}
	}

	return out
}

// structsAllocListStubToApi converts AllocListStub, for CSIPlugin
func structsAllocListStubToApi(alloc *structs.AllocListStub) *api.AllocationListStub {
	out := &api.AllocationListStub{
		ID:                    alloc.ID,
		EvalID:                alloc.EvalID,
		Name:                  alloc.Name,
		Namespace:             alloc.Namespace,
		NodeID:                alloc.NodeID,
		NodeName:              alloc.NodeName,
		JobID:                 alloc.JobID,
		JobType:               alloc.JobType,
		JobVersion:            alloc.JobVersion,
		TaskGroup:             alloc.TaskGroup,
		DesiredStatus:         alloc.DesiredStatus,
		DesiredDescription:    alloc.DesiredDescription,
		ClientStatus:          alloc.ClientStatus,
		ClientDescription:     alloc.ClientDescription,
		FollowupEvalID:        alloc.FollowupEvalID,
		PreemptedAllocations:  alloc.PreemptedAllocations,
		PreemptedByAllocation: alloc.PreemptedByAllocation,
		CreateIndex:           alloc.CreateIndex,
		ModifyIndex:           alloc.ModifyIndex,
		CreateTime:            alloc.CreateTime,
		ModifyTime:            alloc.ModifyTime,
	}

	out.DeploymentStatus = structsAllocDeploymentStatusToApi(alloc.DeploymentStatus)
	out.RescheduleTracker = structsRescheduleTrackerToApi(alloc.RescheduleTracker)

	for k, v := range alloc.TaskStates {
		out.TaskStates[k] = structsTaskStateToApi(v)
	}

	return out
}

// structsAllocDeploymentStatusToApi converts RescheduleTracker, part of AllocListStub
func structsAllocDeploymentStatusToApi(ads *structs.AllocDeploymentStatus) *api.AllocDeploymentStatus {
	out := &api.AllocDeploymentStatus{
		Healthy:     ads.Healthy,
		Timestamp:   ads.Timestamp,
		Canary:      ads.Canary,
		ModifyIndex: ads.ModifyIndex,
	}
	return out
}

// structsRescheduleTrackerToApi converts RescheduleTracker, part of AllocListStub
func structsRescheduleTrackerToApi(rt *structs.RescheduleTracker) *api.RescheduleTracker {
	out := &api.RescheduleTracker{}

	for _, e := range rt.Events {
		out.Events = append(out.Events, &api.RescheduleEvent{
			RescheduleTime: e.RescheduleTime,
			PrevAllocID:    e.PrevAllocID,
			PrevNodeID:     e.PrevNodeID,
		})
	}

	return out
}

// structsTaskStateToApi converts TaskState, part of AllocListStub
func structsTaskStateToApi(ts *structs.TaskState) *api.TaskState {
	out := &api.TaskState{
		State:       ts.State,
		Failed:      ts.Failed,
		Restarts:    ts.Restarts,
		LastRestart: ts.LastRestart,
		StartedAt:   ts.StartedAt,
		FinishedAt:  ts.FinishedAt,
	}

	for _, te := range ts.Events {
		out.Events = append(out.Events, structsTaskEventToApi(te))
	}

	return out
}

// structsTaskEventToApi converts TaskEvents, part of AllocListStub
func structsTaskEventToApi(te *structs.TaskEvent) *api.TaskEvent {
	out := &api.TaskEvent{
		Type:           te.Type,
		Time:           te.Time,
		DisplayMessage: te.DisplayMessage,
		Details:        te.Details,

		// DEPRECATION NOTICE: The following fields are all deprecated. see TaskEvent struct in structs.go for details.
		FailsTask:        te.FailsTask,
		RestartReason:    te.RestartReason,
		SetupError:       te.SetupError,
		DriverError:      te.DriverError,
		DriverMessage:    te.DriverMessage,
		ExitCode:         te.ExitCode,
		Signal:           te.Signal,
		Message:          te.Message,
		KillReason:       te.KillReason,
		KillTimeout:      te.KillTimeout,
		KillError:        te.KillError,
		StartDelay:       te.StartDelay,
		DownloadError:    te.DownloadError,
		ValidationError:  te.ValidationError,
		DiskLimit:        te.DiskLimit,
		FailedSibling:    te.FailedSibling,
		VaultError:       te.VaultError,
		TaskSignalReason: te.TaskSignalReason,
		TaskSignal:       te.TaskSignal,
		GenericSource:    te.GenericSource,

		// DiskSize is in the deprecated section of the api struct but is not in structs
		// DiskSize:         te.DiskSize,
	}

	return out
}
