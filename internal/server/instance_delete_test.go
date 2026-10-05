package server

import (
	"context"
	"errors"
	"testing"

	agentsv1 "github.com/agynio/agents/.gen/go/agynio/api/agents/v1"
	authorizationv1 "github.com/agynio/agents/.gen/go/agynio/api/authorization/v1"
	identityv1 "github.com/agynio/agents/.gen/go/agynio/api/identity/v1"
	"github.com/agynio/agents/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type tupleKey struct{ user, relation, object string }

func keyOf(tuple *authorizationv1.TupleKey) tupleKey {
	return tupleKey{tuple.GetUser(), tuple.GetRelation(), tuple.GetObject()}
}

// tupleAuthorization keeps the tuples DeleteInstance writes and refuses a
// batch that deletes a missing tuple or writes an existing one, as OpenFGA
// does. Computed relations such as can_manage are granted per test.
type tupleAuthorization struct {
	tuples    map[tupleKey]bool
	granted   map[tupleKey]bool
	failWrite error
	writes    int
}

func (a *tupleAuthorization) Check(_ context.Context, req *authorizationv1.CheckRequest, _ ...grpc.CallOption) (*authorizationv1.CheckResponse, error) {
	key := keyOf(req.GetTupleKey())
	return &authorizationv1.CheckResponse{Allowed: a.tuples[key] || a.granted[key]}, nil
}

func (a *tupleAuthorization) Write(_ context.Context, req *authorizationv1.WriteRequest, _ ...grpc.CallOption) (*authorizationv1.WriteResponse, error) {
	a.writes++
	if a.failWrite != nil {
		return nil, a.failWrite
	}
	for _, tuple := range req.GetDeletes() {
		if !a.tuples[keyOf(tuple)] {
			return nil, status.Error(codes.InvalidArgument, "cannot delete a tuple which does not exist")
		}
	}
	for _, tuple := range req.GetWrites() {
		if a.tuples[keyOf(tuple)] {
			return nil, status.Error(codes.InvalidArgument, "write_failed_due_to_invalid_input: tuple already exists")
		}
	}
	for _, tuple := range req.GetDeletes() {
		delete(a.tuples, keyOf(tuple))
	}
	for _, tuple := range req.GetWrites() {
		a.tuples[keyOf(tuple)] = true
	}
	return &authorizationv1.WriteResponse{}, nil
}

// nicknameIdentity applies Identity's rule for writing a nickname
// (authorizeNicknameWrite): an identity may write its own with organization
// membership, and anyone else's only with can_manage_members or
// can_add_member.
type nicknameIdentity struct {
	noopIdentityWriter
	authz     *tupleAuthorization
	nicknames map[string]string
	removedBy []string
	setBy     []string
}

func (f *nicknameIdentity) authorize(ctx context.Context, organizationID, identityID string) (string, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	callers := md.Get("x-identity-id")
	if len(callers) != 1 {
		return "", status.Errorf(codes.Unauthenticated, "expected one caller identity, got %d", len(callers))
	}
	caller := callers[0]
	allowed := func(relation string) bool {
		key := tupleKey{identityPrefix + caller, relation, organizationPrefix + organizationID}
		return f.authz.tuples[key] || f.authz.granted[key]
	}
	if caller == identityID && allowed("member") {
		return caller, nil
	}
	if caller != identityID && (allowed("can_manage_members") || allowed("can_add_member")) {
		return caller, nil
	}
	return "", status.Error(codes.PermissionDenied, "missing permission to manage nickname")
}

func (f *nicknameIdentity) RemoveNickname(ctx context.Context, req *identityv1.RemoveNicknameRequest, _ ...grpc.CallOption) (*identityv1.RemoveNicknameResponse, error) {
	caller, err := f.authorize(ctx, req.GetOrganizationId(), req.GetIdentityId())
	if err != nil {
		return nil, err
	}
	if _, ok := f.nicknames[req.GetIdentityId()]; !ok {
		return nil, status.Error(codes.NotFound, "nickname not found")
	}
	delete(f.nicknames, req.GetIdentityId())
	f.removedBy = append(f.removedBy, caller)
	return &identityv1.RemoveNicknameResponse{}, nil
}

func (f *nicknameIdentity) SetNickname(ctx context.Context, req *identityv1.SetNicknameRequest, _ ...grpc.CallOption) (*identityv1.SetNicknameResponse, error) {
	caller, err := f.authorize(ctx, req.GetOrganizationId(), req.GetIdentityId())
	if err != nil {
		return nil, err
	}
	f.nicknames[req.GetIdentityId()] = req.GetNickname()
	f.setBy = append(f.setBy, caller)
	return &identityv1.SetNicknameResponse{}, nil
}

type fakeInstanceDeletionStore struct {
	instance  store.AgentInstance
	deleteErr error
	deletes   int
}

func (f *fakeInstanceDeletionStore) GetAgentInstance(_ context.Context, id uuid.UUID) (store.AgentInstance, error) {
	if id != f.instance.Meta.ID {
		return store.AgentInstance{}, store.NotFound("agent instance")
	}
	return f.instance, nil
}

func (f *fakeInstanceDeletionStore) DeleteAgentInstance(_ context.Context, id uuid.UUID) (store.AgentInstance, error) {
	f.deletes++
	if f.deleteErr != nil {
		return store.AgentInstance{}, f.deleteErr
	}
	if id != f.instance.Meta.ID || f.instance.State == store.AgentInstanceStateTerminated {
		return store.AgentInstance{}, store.NotFound("agent instance")
	}
	f.instance.State = store.AgentInstanceStateTerminated
	return f.instance, nil
}

type instanceDeletionFixture struct {
	server         *Server
	authz          *tupleAuthorization
	identity       *nicknameIdentity
	instances      *fakeInstanceDeletionStore
	instance       store.AgentInstance
	caller         uuid.UUID
	instanceTuples map[tupleKey]bool
}

// newInstanceDeletionFixture is an active, named instance with the tuples
// addAgentInstanceAuthorization writes, and a caller who is an ordinary
// member of the organization: no can_manage_members, no can_add_member.
func newInstanceDeletionFixture() *instanceDeletionFixture {
	instance := store.AgentInstance{
		Meta:           store.EntityMeta{ID: uuid.New()},
		AgentID:        uuid.New(),
		OrganizationID: uuid.New(),
		Nickname:       "helper",
		Suffix:         "a1b2c3d4",
		State:          store.AgentInstanceStateActive,
	}
	caller := uuid.New()
	instanceTuples := map[tupleKey]bool{}
	for _, tuple := range []*authorizationv1.TupleKey{
		agentInstanceClassTuple(instance.Meta.ID, instance.AgentID),
		agentInstanceOrganizationTuple(instance.Meta.ID, instance.OrganizationID),
		agentInstanceIdentityOrganizationMembershipTuple(instance.Meta.ID, instance.OrganizationID),
		agentInstanceIdentityTuple(instance.Meta.ID, instance.AgentID),
	} {
		instanceTuples[keyOf(tuple)] = true
	}
	tuples := map[tupleKey]bool{
		keyOf(organizationRelationTuple(caller, instance.OrganizationID, "member")): true,
	}
	for key := range instanceTuples {
		tuples[key] = true
	}
	authz := &tupleAuthorization{tuples: tuples, granted: map[tupleKey]bool{}}
	identity := &nicknameIdentity{
		authz:     authz,
		nicknames: map[string]string{instance.Meta.ID.String(): instance.Nickname},
	}
	return &instanceDeletionFixture{
		server:         &Server{authz: authz, identity: identity, notifications: noopNotificationsClient{}},
		authz:          authz,
		identity:       identity,
		instances:      &fakeInstanceDeletionStore{instance: instance},
		instance:       instance,
		caller:         caller,
		instanceTuples: instanceTuples,
	}
}

// grantManage gives the caller can_manage on the instance, which it holds as
// an owner of the agent the instance runs.
func (f *instanceDeletionFixture) grantManage() {
	f.authz.granted[tupleKey{identityPrefix + f.caller.String(), "can_manage", agentInstancePrefix + f.instance.Meta.ID.String()}] = true
}

func (f *instanceDeletionFixture) deleteAsCaller() (*agentsv1.DeleteInstanceResponse, error) {
	return f.server.deleteInstance(identityContext(f.caller), f.instances, &agentsv1.DeleteInstanceRequest{Id: f.instance.Meta.ID.String()})
}

func (f *instanceDeletionFixture) assertInstanceTuples(t *testing.T, present bool) {
	t.Helper()
	for key := range f.instanceTuples {
		if f.authz.tuples[key] != present {
			t.Fatalf("expected tuple %v present=%v", key, present)
		}
	}
}

func (f *instanceDeletionFixture) assertNickname(t *testing.T, present bool) {
	t.Helper()
	if _, ok := f.identity.nicknames[f.instance.Meta.ID.String()]; ok != present {
		t.Fatalf("expected the instance nickname present=%v", present)
	}
}

// The owner of an agent is usually an ordinary member of its organization.
// Removing the nickname as that caller was refused, so such an owner could
// never delete an instance; the instance removes its own nickname instead.
func TestDeleteInstanceByAnOwnerWhoIsOnlyAMember(t *testing.T) {
	f := newInstanceDeletionFixture()
	f.grantManage()

	resp, err := f.deleteAsCaller()
	if err != nil {
		t.Fatalf("expected the owner to delete the instance, got %v", err)
	}
	if resp.GetInstance().GetState() != agentsv1.AgentInstanceState_AGENT_INSTANCE_STATE_TERMINATED {
		t.Fatalf("expected a terminated instance, got %v", resp.GetInstance().GetState())
	}
	f.assertNickname(t, false)
	if len(f.identity.removedBy) != 1 || f.identity.removedBy[0] != f.instance.Meta.ID.String() {
		t.Fatalf("expected the instance to remove its own nickname, got %v", f.identity.removedBy)
	}
	f.assertInstanceTuples(t, false)
	if f.instances.deletes != 1 {
		t.Fatalf("expected one store delete, got %d", f.instances.deletes)
	}
	// Deleting the instance gave the caller nothing on the organization.
	if !f.authz.tuples[keyOf(organizationRelationTuple(f.caller, f.instance.OrganizationID, "member"))] {
		t.Fatal("expected the caller's membership to be untouched")
	}
}

func TestDeleteInstanceRefusesACallerWhoCannotManageIt(t *testing.T) {
	cases := []struct {
		name string
		ctx  func(f *instanceDeletionFixture) context.Context
		code codes.Code
	}{
		{
			name: "an organization member without can_manage",
			ctx:  func(f *instanceDeletionFixture) context.Context { return identityContext(f.caller) },
			code: codes.PermissionDenied,
		},
		{
			// The instance holds membership but not can_manage on itself.
			name: "the instance itself",
			ctx:  func(f *instanceDeletionFixture) context.Context { return identityContext(f.instance.Meta.ID) },
			code: codes.PermissionDenied,
		},
		{
			name: "no caller identity",
			ctx:  func(*instanceDeletionFixture) context.Context { return context.Background() },
			code: codes.Unauthenticated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstanceDeletionFixture()
			_, err := f.server.deleteInstance(tc.ctx(f), f.instances, &agentsv1.DeleteInstanceRequest{Id: f.instance.Meta.ID.String()})
			if status.Code(err) != tc.code {
				t.Fatalf("expected %v, got %v", tc.code, err)
			}
			f.assertNickname(t, true)
			f.assertInstanceTuples(t, true)
			if f.authz.writes != 0 || f.instances.deletes != 0 || len(f.identity.removedBy) != 0 {
				t.Fatalf("expected nothing changed, got writes=%d deletes=%d removed=%v", f.authz.writes, f.instances.deletes, f.identity.removedBy)
			}
		})
	}
}

// A retry after a failure that left the nickname removed completes the
// deletion rather than failing on the missing nickname forever.
func TestDeleteInstanceCompletesWhenTheNicknameIsAlreadyGone(t *testing.T) {
	f := newInstanceDeletionFixture()
	f.grantManage()
	delete(f.identity.nicknames, f.instance.Meta.ID.String())

	if _, err := f.deleteAsCaller(); err != nil {
		t.Fatalf("expected the deletion to complete, got %v", err)
	}
	f.assertInstanceTuples(t, false)
	if f.instances.instance.State != store.AgentInstanceStateTerminated {
		t.Fatalf("expected a terminated instance, got %q", f.instances.instance.State)
	}
}

func TestDeleteInstanceRestoresTheNicknameWhenTheTuplesStay(t *testing.T) {
	f := newInstanceDeletionFixture()
	f.grantManage()
	f.authz.failWrite = errors.New("authorization unavailable")

	_, err := f.deleteAsCaller()
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected an internal error, got %v", err)
	}
	f.assertNickname(t, true)
	if len(f.identity.setBy) != 1 || f.identity.setBy[0] != f.instance.Meta.ID.String() {
		t.Fatalf("expected the instance to restore its own nickname, got %v", f.identity.setBy)
	}
	f.assertInstanceTuples(t, true)
	if f.instances.deletes != 0 {
		t.Fatalf("expected no store delete, got %d", f.instances.deletes)
	}
}

// The rollback must restore the instance that was read, not the empty record
// a failed delete returns.
func TestDeleteInstanceRollsBackWhenTheStoreFails(t *testing.T) {
	f := newInstanceDeletionFixture()
	f.grantManage()
	f.instances.deleteErr = errors.New("database unavailable")

	_, err := f.deleteAsCaller()
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected an internal error, got %v", err)
	}
	f.assertInstanceTuples(t, true)
	f.assertNickname(t, true)
	if len(f.identity.setBy) != 1 || f.identity.setBy[0] != f.instance.Meta.ID.String() {
		t.Fatalf("expected the instance to restore its own nickname, got %v", f.identity.setBy)
	}
	if f.instances.instance.State != store.AgentInstanceStateActive {
		t.Fatalf("expected the instance to stay active, got %q", f.instances.instance.State)
	}

	// The same call succeeds once the store recovers.
	f.instances.deleteErr = nil
	if _, err := f.deleteAsCaller(); err != nil {
		t.Fatalf("expected the retry to delete the instance, got %v", err)
	}
	f.assertInstanceTuples(t, false)
	f.assertNickname(t, false)
}

func TestDeleteInstanceOfATerminatedInstanceChangesNothing(t *testing.T) {
	f := newInstanceDeletionFixture()
	f.grantManage()
	f.instances.instance.State = store.AgentInstanceStateTerminated

	resp, err := f.deleteAsCaller()
	if err != nil {
		t.Fatalf("expected a repeated delete to succeed, got %v", err)
	}
	if resp.GetInstance().GetState() != agentsv1.AgentInstanceState_AGENT_INSTANCE_STATE_TERMINATED {
		t.Fatalf("expected a terminated instance, got %v", resp.GetInstance().GetState())
	}
	if f.authz.writes != 0 || f.instances.deletes != 0 || len(f.identity.removedBy) != 0 {
		t.Fatalf("expected nothing changed, got writes=%d deletes=%d removed=%v", f.authz.writes, f.instances.deletes, f.identity.removedBy)
	}
}
