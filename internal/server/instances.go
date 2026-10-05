package server

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"

	agentsv1 "github.com/agynio/agents/.gen/go/agynio/api/agents/v1"
	identityv1 "github.com/agynio/agents/.gen/go/agynio/api/identity/v1"
	"github.com/agynio/agents/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const generatedInstanceSuffixLength = 8

var instanceSuffixPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func (s *Server) CreateInstance(ctx context.Context, req *agentsv1.CreateInstanceRequest) (*agentsv1.CreateInstanceResponse, error) {
	agentID, err := parseUUID(req.GetAgentId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent_id: %v", err)
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, toStatusError(err)
	}
	callerID, err := identityIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	callerUUID, err := parseUUID(callerID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "identity_id: %v", err)
	}
	if err := s.requireAgentCanInitiate(ctx, callerUUID, agentID); err != nil {
		return nil, err
	}
	if agent.Nickname == "" {
		return nil, status.Error(codes.FailedPrecondition, "agent nickname is required to create an instance")
	}
	label := req.Label
	if label != nil && !instanceSuffixPattern.MatchString(req.GetLabel()) {
		return nil, status.Errorf(codes.InvalidArgument, "label must match %s", instanceSuffixPattern.String())
	}
	defaultThreadID, err := resolveDefaultThread(agent, req)
	if err != nil {
		return nil, err
	}
	instance, err := s.createInstanceWithIdentity(ctx, agent, label, defaultThreadID)
	if err != nil {
		return nil, err
	}
	return &agentsv1.CreateInstanceResponse{Instance: toProtoAgentInstance(instance)}, nil
}

// resolveDefaultThread decides where this instance's untargeted messages will
// go. Both creation paths funnel through here, and the class definition is
// already loaded, so the policy is applied here rather than in Threads.
//
// An explicit default_thread_id is a deliberate act by a caller who knows the
// destination, so it wins over the policy -- which governs only what the
// platform infers when nobody said.
func resolveDefaultThread(agent store.Agent, req *agentsv1.CreateInstanceRequest) (*uuid.UUID, error) {
	if raw := strings.TrimSpace(req.GetDefaultThreadId()); raw != "" {
		id, err := parseUUID(raw)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "default_thread_id: %v", err)
		}
		return &id, nil
	}
	if agent.DefaultThread == store.AgentDefaultThreadNone {
		return nil, nil
	}
	raw := strings.TrimSpace(req.GetContext().GetThreadId())
	if raw == "" {
		return nil, nil
	}
	id, err := parseUUID(raw)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "context.thread_id: %v", err)
	}
	return &id, nil
}

// agentInstanceInput assembles the row an instance is created from.
//
// Extracted so the resolved default thread cannot go missing again: it was a
// parameter of the caller and simply never reached this struct, so every
// instance was written with a null default_thread_id no matter what the class
// policy resolved or the caller asked for.
func agentInstanceInput(agent store.Agent, label *string, suffix string, defaultThreadID *uuid.UUID) store.AgentInstanceInput {
	return store.AgentInstanceInput{
		AgentID:         agent.Meta.ID,
		OrganizationID:  agent.OrganizationID,
		Label:           label,
		Suffix:          suffix,
		Nickname:        agent.Nickname,
		DefaultThreadID: defaultThreadID,
	}
}

func (s *Server) createInstanceWithIdentity(ctx context.Context, agent store.Agent, label *string, defaultThreadID *uuid.UUID) (store.AgentInstance, error) {
	attempts := 1
	if label == nil {
		attempts = 5
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		suffix := generatedInstanceSuffix()
		if label != nil {
			suffix = *label
		}
		instance, err := s.store.CreateAgentInstance(ctx, agentInstanceInput(agent, label, suffix, defaultThreadID))
		if err != nil {
			if _, ok := err.(*store.AlreadyExistsError); ok && label == nil {
				lastErr = err
				continue
			}
			return store.AgentInstance{}, toStatusError(err)
		}
		if err := s.addAgentInstanceAuthorization(ctx, instance); err != nil {
			rollbackErr := s.rollbackCreatedInstance(ctx, instance)
			if rollbackErr != nil {
				return store.AgentInstance{}, status.Errorf(codes.Internal, "authorization write failed: %v; rollback: %v", err, rollbackErr)
			}
			return store.AgentInstance{}, status.Errorf(codes.Internal, "authorization write failed: %v", err)
		}
		if err := s.registerAgentInstanceIdentity(ctx, instance.Meta.ID); err != nil {
			rollbackErr := errors.Join(
				s.removeAgentInstanceAuthorization(ctx, instance),
				s.rollbackCreatedInstance(ctx, instance),
			)
			if rollbackErr != nil {
				return store.AgentInstance{}, status.Errorf(codes.Internal, "register instance identity: %v; rollback: %v", err, rollbackErr)
			}
			return store.AgentInstance{}, status.Errorf(codes.Internal, "register instance identity: %v", err)
		}
		if err := s.setAgentInstanceNickname(ctx, instance); err != nil {
			rollbackErr := errors.Join(
				s.removeAgentInstanceAuthorization(ctx, instance),
				s.rollbackCreatedInstance(ctx, instance),
			)
			if rollbackErr != nil {
				return store.AgentInstance{}, status.Errorf(codes.Internal, "set instance nickname: %v; rollback: %v", err, rollbackErr)
			}
			return store.AgentInstance{}, status.Errorf(codes.Internal, "set instance nickname: %v", err)
		}
		s.publishInstanceUpdated(ctx, instance)
		return instance, nil
	}
	return store.AgentInstance{}, toStatusError(lastErr)
}

func generatedInstanceSuffix() string {
	return uuid.NewString()[:generatedInstanceSuffixLength]
}

func (s *Server) rollbackCreatedInstance(ctx context.Context, instance store.AgentInstance) error {
	_, err := s.store.DeleteAgentInstance(ctx, instance.Meta.ID)
	return err
}

func (s *Server) registerAgentInstanceIdentity(ctx context.Context, instanceID uuid.UUID) error {
	_, err := s.identity.RegisterIdentity(ctx, &identityv1.RegisterIdentityRequest{
		IdentityId:   instanceID.String(),
		IdentityType: identityv1.IdentityType_IDENTITY_TYPE_AGENT_INSTANCE,
	})
	return err
}

// setAgentInstanceNickname names the instance as the instance, not as whoever
// asked for it.
//
// Identity lets a caller set its own nickname with plain organization
// membership, and anyone else's only with can_manage_members. An instance is
// created from a thread, so the caller is an ordinary participant who has
// neither -- and the Orchestrator reaches this with no caller at all. The
// instance itself holds an organization membership tuple by this point
// (addAgentInstanceAuthorization runs first), so it is the one caller that is
// always allowed.
func (s *Server) setAgentInstanceNickname(ctx context.Context, instance store.AgentInstance) error {
	identityCtx := metadata.AppendToOutgoingContext(ctx, "x-identity-id", instance.Meta.ID.String())
	_, err := s.identity.SetNickname(identityCtx, &identityv1.SetNicknameRequest{
		OrganizationId: instance.OrganizationID.String(),
		IdentityId:     instance.Meta.ID.String(),
		Nickname:       instance.Nickname,
		InstanceSuffix: &instance.Suffix,
	})
	return err
}

func (s *Server) GetInstance(ctx context.Context, req *agentsv1.GetInstanceRequest) (*agentsv1.GetInstanceResponse, error) {
	id, err := parseUUID(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	instance, err := s.store.GetAgentInstance(ctx, id)
	if err != nil {
		return nil, toStatusError(err)
	}
	if err := s.requireInstanceReadAccess(ctx, instance); err != nil {
		return nil, err
	}
	return &agentsv1.GetInstanceResponse{Instance: toProtoAgentInstance(instance)}, nil
}

// requireInstanceReadAccess authorizes reading one instance on the same terms
// ListInstances reads many: an identified caller must belong to the instance's
// organization. An internal caller holds no identity and is served -- threads
// and the orchestrator resolve instances on their own behalf. An instance may
// always read itself, which no organization membership would grant it.
func (s *Server) requireInstanceReadAccess(ctx context.Context, instance store.AgentInstance) error {
	identityID, hasIdentity, err := optionalIdentityUUIDFromContext(ctx)
	if err != nil {
		return err
	}
	if !hasIdentity || identityID == instance.Meta.ID {
		return nil
	}
	return s.requireOrganizationMember(ctx, identityID, instance.OrganizationID)
}

// SetInstanceDefaultThread moves where an instance's untargeted messages go.
// The class policy governs only what the platform infers at creation; naming a
// thread afterwards is a deliberate act, so it is allowed whatever the class
// asked for. Unsetting leaves the instance with no destination.
func (s *Server) SetInstanceDefaultThread(ctx context.Context, req *agentsv1.SetInstanceDefaultThreadRequest) (*agentsv1.SetInstanceDefaultThreadResponse, error) {
	id, err := parseUUID(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := s.requireManageInstance(ctx, id); err != nil {
		return nil, err
	}
	var threadID *uuid.UUID
	if raw := strings.TrimSpace(req.GetDefaultThreadId()); raw != "" {
		parsed, err := parseUUID(raw)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "default_thread_id: %v", err)
		}
		threadID = &parsed
	}
	instance, err := s.store.SetAgentInstanceDefaultThread(ctx, id, threadID)
	if err != nil {
		return nil, toStatusError(err)
	}
	s.publishInstanceUpdated(ctx, instance)
	return &agentsv1.SetInstanceDefaultThreadResponse{Instance: toProtoAgentInstance(instance)}, nil
}

func (s *Server) ListInstances(ctx context.Context, req *agentsv1.ListInstancesRequest) (*agentsv1.ListInstancesResponse, error) {
	organizationID, err := s.organizationListScope(ctx, req.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	cursor, err := decodePageCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid page_token: %v", err)
	}
	filter := store.AgentInstanceFilter{OrganizationID: organizationID}
	if req.AgentId != nil {
		agentID, err := parseUUID(req.GetAgentId())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "agent_id: %v", err)
		}
		filter.AgentID = &agentID
	}
	for _, protoState := range req.GetStateIn() {
		state, err := agentInstanceStateFromProto(protoState)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "state_in: %v", err)
		}
		filter.StateIn = append(filter.StateIn, state)
	}
	if req.HasUnacked != nil {
		filter.HasUnacked = req.HasUnacked
	}
	result, err := s.store.ListAgentInstances(ctx, filter, req.GetPageSize(), cursor)
	if err != nil {
		return nil, toStatusError(err)
	}
	instances, nextToken := mapListResult(result.Instances, result.NextCursor, toProtoAgentInstance)
	return &agentsv1.ListInstancesResponse{Instances: instances, NextPageToken: nextToken}, nil
}

// PauseInstance stops an instance from spawning workloads. Two callers reach
// it, and only one of them is a principal: a user, app or agent pausing an
// instance deliberately holds can_manage, while the Orchestrator pauses on the
// platform's behalf -- the runner was deprovisioned, the volume is gone, start
// retries are spent -- and has no identity to hold anything. Requiring
// can_manage of both left the Orchestrator unable to record any of those, since
// an instance is not an owner of its own class. An AuthorizationPolicy narrows
// the unidentified path to the Orchestrator.
func (s *Server) PauseInstance(ctx context.Context, req *agentsv1.PauseInstanceRequest) (*agentsv1.PauseInstanceResponse, error) {
	id, err := parseUUID(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if req.GetPauseReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "pause_reason must be provided")
	}
	if err := s.requireManageInstanceUnlessInternal(ctx, id); err != nil {
		return nil, err
	}
	instance, err := s.store.PauseAgentInstance(ctx, id, req.GetPauseReason())
	if err != nil {
		return nil, toStatusError(err)
	}
	s.publishInstanceUpdated(ctx, instance)
	return &agentsv1.PauseInstanceResponse{Instance: toProtoAgentInstance(instance)}, nil
}

func (s *Server) ResumeInstance(ctx context.Context, req *agentsv1.ResumeInstanceRequest) (*agentsv1.ResumeInstanceResponse, error) {
	id, err := parseUUID(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := s.requireManageInstance(ctx, id); err != nil {
		return nil, err
	}
	instance, err := s.store.ResumeAgentInstance(ctx, id)
	if err != nil {
		return nil, toStatusError(err)
	}
	s.publishInstanceUpdated(ctx, instance)
	return &agentsv1.ResumeInstanceResponse{Instance: toProtoAgentInstance(instance)}, nil
}

func (s *Server) DeleteInstance(ctx context.Context, req *agentsv1.DeleteInstanceRequest) (*agentsv1.DeleteInstanceResponse, error) {
	return s.deleteInstance(ctx, s.store, req)
}

// instanceDeletionStore is the store behaviour deleting an instance needs.
type instanceDeletionStore interface {
	GetAgentInstance(ctx context.Context, id uuid.UUID) (store.AgentInstance, error)
	DeleteAgentInstance(ctx context.Context, id uuid.UUID) (store.AgentInstance, error)
}

// deleteInstance terminates an instance for a caller who holds can_manage on
// it, and undoes each completed step if a later one fails.
//
// The instance's nickname is removed first and as the instance itself, the
// same way it was set (setAgentInstanceNickname). Identity lets an identity
// remove its own nickname with plain organization membership, and anyone
// else's only with can_add_member or can_manage_members. Removing it as the
// caller refused every owner who is an ordinary member, after the instance's
// tuples had already been removed and had to be restored. Removing it as the
// instance grants the caller nothing new: can_manage on the instance is still
// required, and the instance can name only itself. The instance is a member
// only through the tuple removeAgentInstanceAuthorization deletes, so the
// nickname has to go before the tuples do.
//
// The store is taken as an interface so the sequence and its rollbacks can be
// tested without a database.
func (s *Server) deleteInstance(ctx context.Context, instances instanceDeletionStore, req *agentsv1.DeleteInstanceRequest) (*agentsv1.DeleteInstanceResponse, error) {
	id, err := parseUUID(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := s.requireManageInstance(ctx, id); err != nil {
		return nil, err
	}
	instance, err := instances.GetAgentInstance(ctx, id)
	if err != nil {
		return nil, toStatusError(err)
	}
	if instance.State == store.AgentInstanceStateTerminated {
		return &agentsv1.DeleteInstanceResponse{Instance: toProtoAgentInstance(instance)}, nil
	}
	removedNickname, err := s.removeAgentInstanceNickname(ctx, instance)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove instance nickname: %v", err)
	}
	// Restores only what this call removed: a nickname that was already gone
	// stays gone.
	restoreNickname := func() error {
		if !removedNickname {
			return nil
		}
		return s.setAgentInstanceNickname(ctx, instance)
	}
	if err := s.removeAgentInstanceAuthorization(ctx, instance); err != nil {
		if rollbackErr := restoreNickname(); rollbackErr != nil {
			return nil, status.Errorf(codes.Internal, "authorization delete failed: %v; rollback: %v", err, rollbackErr)
		}
		return nil, status.Errorf(codes.Internal, "authorization delete failed: %v", err)
	}
	deleted, err := instances.DeleteAgentInstance(ctx, id)
	if err != nil {
		// The membership tuple comes back first: the instance restores its own
		// nickname, which needs it. The rollback reuses the instance read
		// above; a failed delete returns no record.
		rollbackErr := s.addAgentInstanceAuthorization(ctx, instance)
		rollbackErr = errors.Join(rollbackErr, restoreNickname())
		if rollbackErr != nil {
			return nil, status.Errorf(codes.Internal, "delete instance failed: %v; rollback: %v", err, rollbackErr)
		}
		return nil, toStatusError(err)
	}
	s.publishInstanceUpdated(ctx, deleted)
	return &agentsv1.DeleteInstanceResponse{Instance: toProtoAgentInstance(deleted)}, nil
}

// removeAgentInstanceNickname releases the instance's handle as the instance,
// for the reason setAgentInstanceNickname names it as the instance. It reports
// whether this call removed it: a nickname that is already gone -- a retry
// after a failure that left it removed -- is not an error.
func (s *Server) removeAgentInstanceNickname(ctx context.Context, instance store.AgentInstance) (bool, error) {
	identityCtx := metadata.AppendToOutgoingContext(ctx, "x-identity-id", instance.Meta.ID.String())
	_, err := s.identity.RemoveNickname(identityCtx, &identityv1.RemoveNicknameRequest{
		OrganizationId: instance.OrganizationID.String(),
		IdentityId:     instance.Meta.ID.String(),
	})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// requireManageInstanceUnlessInternal authorizes a lifecycle change that the
// platform itself makes as well as its owner. An internal caller holds no
// identity, the same signal FanoutInboxItem and the instance reads already use;
// anyone who presents one is a principal and is held to can_manage.
func (s *Server) requireManageInstanceUnlessInternal(ctx context.Context, id uuid.UUID) error {
	_, hasIdentity, err := optionalIdentityUUIDFromContext(ctx)
	if err != nil {
		return err
	}
	if !hasIdentity {
		return nil
	}
	return s.requireManageInstance(ctx, id)
}

func (s *Server) requireManageInstance(ctx context.Context, id uuid.UUID) error {
	callerID, err := identityIDFromContext(ctx)
	if err != nil {
		return err
	}
	callerUUID, err := parseUUID(callerID)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "identity_id: %v", err)
	}
	return s.requireAgentInstanceCanManage(ctx, callerUUID, id)
}

func (s *Server) WriteInboxItem(ctx context.Context, req *agentsv1.WriteInboxItemRequest) (*agentsv1.WriteInboxItemResponse, error) {
	input, err := directInboxInputFromRequest(req)
	if err != nil {
		return nil, err
	}
	callerID, err := identityIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	callerUUID, err := parseUUID(callerID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "identity_id: %v", err)
	}
	if input.SenderID != callerUUID {
		return nil, status.Error(codes.PermissionDenied, "sender_id must match caller identity")
	}
	if err := s.requireAgentInstanceCanWriteInbox(ctx, callerUUID, input.AgentInstanceID); err != nil {
		return nil, err
	}
	item, err := s.store.CreateInboxItem(ctx, input)
	if err != nil {
		return nil, toStatusError(err)
	}
	s.publishInboxItemCreated(ctx, item)
	return &agentsv1.WriteInboxItemResponse{Item: toProtoInboxItem(item)}, nil
}

// FanoutInboxItem writes an item on behalf of whoever sent the message, so it
// takes sender_id and thread_id from the caller rather than deriving them.
// Threads calls it over the mesh with no identity of its own; a caller that
// presents one is a user, app or agent, and must go through WriteInboxItem,
// which checks can_write_inbox and that the sender is the caller. Without this
// any authenticated caller could forge an item from any sender on any thread.
// An AuthorizationPolicy narrows the unauthenticated path to Threads.
func (s *Server) FanoutInboxItem(ctx context.Context, req *agentsv1.FanoutInboxItemRequest) (*agentsv1.FanoutInboxItemResponse, error) {
	if _, hasIdentity, err := optionalIdentityUUIDFromContext(ctx); err != nil {
		return nil, err
	} else if hasIdentity {
		return nil, status.Error(codes.PermissionDenied, "fanout is internal; use WriteInboxItem")
	}
	input, err := fanoutInboxInputFromRequest(req)
	if err != nil {
		return nil, err
	}
	item, inserted, err := s.store.FanoutInboxItem(ctx, input)
	if err != nil {
		return nil, toStatusError(err)
	}
	if inserted {
		s.publishInboxItemCreated(ctx, item)
	}
	return &agentsv1.FanoutInboxItemResponse{Item: toProtoInboxItem(item)}, nil
}

func (s *Server) GetUnackedInboxItems(ctx context.Context, req *agentsv1.GetUnackedInboxItemsRequest) (*agentsv1.GetUnackedInboxItemsResponse, error) {
	instanceID, err := parseUUID(req.GetAgentInstanceId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent_instance_id: %v", err)
	}
	if err := s.requireSelfInstanceOrPlatform(ctx, instanceID); err != nil {
		return nil, err
	}
	cursor, err := decodeInboxPageCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid page_token: %v", err)
	}
	result, err := s.store.ListUnackedInboxItems(ctx, instanceID, req.GetPageSize(), cursor)
	if err != nil {
		return nil, toStatusError(err)
	}
	items, nextToken := mapInboxListResult(result.Items, result.NextCursor, toProtoInboxItem)
	s.attachSenderHandles(ctx, instanceID, items)
	return &agentsv1.GetUnackedInboxItemsResponse{Items: items, NextPageToken: nextToken}, nil
}

// attachSenderHandles names each sender, because the reader is an agent
// workload and the Identity service is not reachable from one. A failure leaves
// the handles empty rather than failing the read: the sender id still stands.
func (s *Server) attachSenderHandles(ctx context.Context, instanceID uuid.UUID, items []*agentsv1.InboxItem) {
	if len(items) == 0 {
		return
	}
	instance, err := s.store.GetAgentInstance(ctx, instanceID)
	if err != nil {
		log.Printf("agents: read instance %s for sender handles: %v", instanceID, err)
		return
	}
	identityCtx, err := identityOutgoingContext(ctx)
	if err != nil {
		log.Printf("agents: forward identity for sender handles: %v", err)
		return
	}
	entries, err := s.senderHandles(identityCtx, instance.OrganizationID.String(), items)
	if err != nil {
		log.Printf("agents: resolve sender handles for instance %s: %v", instanceID, err)
		return
	}
	s.applySenderHandles(items, entries)
}

func (s *Server) senderHandles(ctx context.Context, organizationID string, items []*agentsv1.InboxItem) ([]*identityv1.NicknameEntry, error) {
	senderIDs := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		senderID := item.GetSenderId()
		if senderID == "" {
			continue
		}
		if _, ok := seen[senderID]; ok {
			continue
		}
		seen[senderID] = struct{}{}
		senderIDs = append(senderIDs, senderID)
	}
	if len(senderIDs) == 0 {
		return nil, nil
	}
	response, err := s.identity.BatchGetNicknames(ctx, &identityv1.BatchGetNicknamesRequest{
		OrganizationId: organizationID,
		IdentityIds:    senderIDs,
	})
	if err != nil {
		return nil, err
	}
	return response.GetEntries(), nil
}

func (s *Server) applySenderHandles(items []*agentsv1.InboxItem, entries []*identityv1.NicknameEntry) {
	handles := make(map[string]string, len(entries))
	for _, entry := range entries {
		handle := entry.GetNickname()
		if handle == "" {
			continue
		}
		if suffix := entry.GetInstanceSuffix(); suffix != "" {
			handle += "#" + suffix
		}
		handles[entry.GetIdentityId()] = handle
	}
	for _, item := range items {
		if handle, ok := handles[item.GetSenderId()]; ok {
			value := handle
			item.SenderHandle = &value
		}
	}
}

func (s *Server) AckInboxItems(ctx context.Context, req *agentsv1.AckInboxItemsRequest) (*agentsv1.AckInboxItemsResponse, error) {
	instanceID, err := parseUUID(req.GetAgentInstanceId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent_instance_id: %v", err)
	}
	if err := requireSelfInstance(ctx, instanceID); err != nil {
		return nil, err
	}
	itemIDs, err := parseUUIDList(req.GetItemIds(), "item_ids")
	if err != nil {
		return nil, err
	}
	ackedCount, err := s.store.AckInboxItems(ctx, instanceID, itemIDs)
	if err != nil {
		return nil, toStatusError(err)
	}
	return &agentsv1.AckInboxItemsResponse{AckedCount: ackedCount}, nil
}

func (s *Server) GetUnackedInboxCount(ctx context.Context, req *agentsv1.GetUnackedInboxCountRequest) (*agentsv1.GetUnackedInboxCountResponse, error) {
	instanceID, err := parseUUID(req.GetAgentInstanceId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent_instance_id: %v", err)
	}
	if err := s.requireSelfInstanceOrPlatform(ctx, instanceID); err != nil {
		return nil, err
	}
	var threadID *uuid.UUID
	if req.ThreadId != nil {
		parsedThreadID, err := parseUUID(req.GetThreadId())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "thread_id: %v", err)
		}
		threadID = &parsedThreadID
	}
	count, err := s.store.CountUnackedInboxItems(ctx, instanceID, threadID)
	if err != nil {
		return nil, toStatusError(err)
	}
	return &agentsv1.GetUnackedInboxCountResponse{Count: count}, nil
}

func directInboxInputFromRequest(req *agentsv1.WriteInboxItemRequest) (store.InboxItemInput, error) {
	instanceID, err := parseUUID(req.GetAgentInstanceId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "agent_instance_id: %v", err)
	}
	senderID, err := parseUUID(req.GetSenderId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "sender_id: %v", err)
	}
	fileIDs, err := parseUUIDList(req.GetFileIds(), "file_ids")
	if err != nil {
		return store.InboxItemInput{}, err
	}
	return store.InboxItemInput{
		AgentInstanceID: instanceID,
		SourceKind:      store.InboxItemSourceKindDirect,
		SenderID:        senderID,
		Body:            req.GetBody(),
		FileIDs:         fileIDs,
	}, nil
}

func fanoutInboxInputFromRequest(req *agentsv1.FanoutInboxItemRequest) (store.InboxItemInput, error) {
	instanceID, err := parseUUID(req.GetAgentInstanceId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "agent_instance_id: %v", err)
	}
	threadID, err := parseUUID(req.GetThreadId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "thread_id: %v", err)
	}
	messageID, err := parseUUID(req.GetMessageId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "message_id: %v", err)
	}
	senderID, err := parseUUID(req.GetSenderId())
	if err != nil {
		return store.InboxItemInput{}, status.Errorf(codes.InvalidArgument, "sender_id: %v", err)
	}
	fileIDs, err := parseUUIDList(req.GetFileIds(), "file_ids")
	if err != nil {
		return store.InboxItemInput{}, err
	}
	return store.InboxItemInput{
		AgentInstanceID: instanceID,
		SourceKind:      store.InboxItemSourceKindThread,
		ThreadID:        &threadID,
		MessageID:       &messageID,
		SenderID:        senderID,
		Body:            req.GetBody(),
		FileIDs:         fileIDs,
	}, nil
}

func parseUUIDList(values []string, field string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(values))
	for i, value := range values {
		id, err := parseUUID(value)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s[%d]: %v", field, i, err)
		}
		ids[i] = id
	}
	return ids, nil
}

func requireSelfInstance(ctx context.Context, instanceID uuid.UUID) error {
	identityID, err := identityIDFromContext(ctx)
	if err != nil {
		return err
	}
	if identityID != instanceID.String() {
		return status.Error(codes.PermissionDenied, "caller must be the agent instance")
	}
	return nil
}

// requireSelfInstanceOrPlatform admits the instance itself, or the platform.
//
// The Orchestrator has to know which thread an instance was handed work for
// before it can place a workload to do it, and it is not that instance. It used
// to send the instance's own id as the caller to get past the check above,
// which is a platform service claiming to be the thing it manages. As itself it
// carries cluster admin, and that is what admits it here.
//
// Reads only. Acking is the instance saying it has handled its own message and
// stays exactly that -- see AckInboxItems, which keeps requireSelfInstance.
func (s *Server) requireSelfInstanceOrPlatform(ctx context.Context, instanceID uuid.UUID) error {
	selfErr := requireSelfInstance(ctx, instanceID)
	if selfErr == nil {
		return nil
	}
	if identityTypeFromContext(ctx) != platformIdentityType {
		return selfErr
	}
	identityID, err := identityIDFromContext(ctx)
	if err != nil {
		return selfErr
	}
	callerID, err := uuid.Parse(identityID)
	if err != nil {
		return selfErr
	}
	if err := s.requireAllowed(ctx, callerID, clusterAdminRelation, clusterObject, selfErr); err != nil {
		return err
	}
	return nil
}
