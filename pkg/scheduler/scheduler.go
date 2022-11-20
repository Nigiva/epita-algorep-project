package scheduler

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Timelessprod/algorep/pkg/core"
	"github.com/Timelessprod/algorep/pkg/utils"
	"go.uber.org/zap"
)

var logger *zap.Logger = core.Logger

/********************
 ** Scheduler Node **
 ********************/

// SchedulerNode is the node in charge of scheduling the database entries with the RAFT algorithm
type SchedulerNode struct {
	Id          uint32
	Card        core.NodeCard
	State       core.State
	CurrentTerm uint32
	LeaderId    int

	VotedFor        int32
	ElectionTimeout time.Duration
	VoteCount       uint32

	// Each entry contains command for state machine
	// and term when entry was received by leader (first index is 1)
	log map[uint32]core.Entry
	// Job id counter
	jobIdCounter uint32
	// Index of highest log entry known to be committed (initialized to 0, increases monotonically)
	commitIndex uint32
	// Index of highest log entry known to be replicated on other nodes (initialized to 0, increases monotonically)
	matchIndex []uint32
	// Index of highest log entry available to store next entry (initialized to 1, increases monotonically)
	nextIndex []uint32

	Channel core.ChannelContainer

	IsStarted bool
	IsCrashed bool

	// State file to debug the node state
	StateFile *os.File
}

// Init the scheduler node
func (node *SchedulerNode) Init(id uint32) SchedulerNode {
	node.Id = id
	node.Card = core.NodeCard{Id: id, Type: core.SchedulerNodeType}
	node.State = core.FollowerState
	node.LeaderId = core.NO_NODE
	node.VotedFor = core.NO_NODE
	node.VoteCount = 0

	// Compute random leaderIsAliveTimeout
	durationRange := int(core.Config.MaxElectionTimeout - core.Config.MinElectionTimeout)
	node.ElectionTimeout = time.Duration(rand.Intn(durationRange)) + core.Config.MinElectionTimeout

	// Initialize all elements used to store and replicate the log
	node.log = make(map[uint32]core.Entry)
	node.jobIdCounter = 0
	node.commitIndex = 0
	node.matchIndex = make([]uint32, core.Config.SchedulerNodeCount)
	for i := range node.matchIndex {
		node.matchIndex[i] = 0
	}
	node.nextIndex = make([]uint32, core.Config.SchedulerNodeCount)
	for i := range node.nextIndex {
		node.nextIndex[i] = 1
	}

	// Initialize the channel container
	node.Channel.RequestCommand = make(chan core.RequestCommandRPC, core.Config.ChannelBufferSize)
	node.Channel.ResponseCommand = make(chan core.ResponseCommandRPC, core.Config.ChannelBufferSize)
	node.Channel.RequestVote = make(chan core.RequestVoteRPC, core.Config.ChannelBufferSize)
	node.Channel.ResponseVote = make(chan core.ResponseVoteRPC, core.Config.ChannelBufferSize)

	// Initialize the state file
	node.InitStateInFile()

	logger.Info("Node initialized", zap.Uint32("id", id))
	return *node
}

// Run the scheduler node
func (node *SchedulerNode) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	defer node.StateFile.Close()
	logger.Info("Node is waiting the START command from REPL", zap.String("Node", node.Card.String()))
	// Wait for the start command from REPL and listen to the channel RequestCommand
	for !node.IsStarted {
		select {
		case request := <-node.Channel.RequestCommand:
			node.handleRequestCommandRPC(request)
		}
	}
	logger.Info("Node started", zap.String("Node", node.Card.String()))

	for {
		select {
		case request := <-node.Channel.RequestCommand:
			node.handleRequestCommandRPC(request)
		case response := <-node.Channel.ResponseCommand:
			node.handleResponseCommandRPC(response)
		case request := <-node.Channel.RequestVote:
			node.handleRequestVoteRPC(request)
		case response := <-node.Channel.ResponseVote:
			node.handleResponseVoteRPC(response)
		case <-time.After(node.getTimeOut()):
			node.handleTimeout()
		}
		node.printNodeStateInFile()
		node.updateCommitIndex()
		time.Sleep(core.Config.NodeSpeedList[node.Id])
	}
}

func (node *SchedulerNode) InitStateInFile() {
	path := fmt.Sprintf("state/%d.node", node.Id)
	os.MkdirAll(filepath.Dir(path), os.ModePerm)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logger.Error("Error while creating state file",
			zap.String("Node", node.Card.String()),
			zap.Error(err),
		)
		return
	}
	node.StateFile = f
}

func (node *SchedulerNode) printNodeStateInFile() {
	if node.StateFile == nil {
		return
	}
	f := node.StateFile
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintln(f, "--- ", node.Card.String(), " ---")
	fmt.Fprintln(f, ">>> State: ", node.State)
	fmt.Fprintln(f, ">>> IsCrashed: ", node.IsCrashed)
	fmt.Fprintln(f, ">>> CurrentTerm: ", node.CurrentTerm)
	fmt.Fprintln(f, ">>> LeaderId: ", node.LeaderId)
	fmt.Fprintln(f, ">>> VotedFor: ", node.VotedFor)
	fmt.Fprintln(f, ">>> ElectionTimeout: ", node.ElectionTimeout)
	fmt.Fprintln(f, ">>> VoteCount: ", node.VoteCount)
	fmt.Fprintln(f, ">>> CommitIndex: ", node.commitIndex)
	fmt.Fprintln(f, ">>> MatchIndex: ", node.matchIndex)
	fmt.Fprintln(f, ">>> NextIndex: ", node.nextIndex)
	fmt.Fprintln(f, "### Log ###")
	for i, entry := range node.log {
		fmt.Fprintf(f, "[%v] Job %v | Worker %v | %v\n", i, entry.Job.GetReference(), entry.WorkerId, entry.Job.Status.String())
	}
	fmt.Fprintln(f, "----------------")
}

// Add a new entry to the log
func (node *SchedulerNode) addEntryToLog(entry core.Entry) {
	index := node.nextIndex[node.Card.Id]
	node.log[index] = entry
	node.nextIndex[node.Card.Id] = index + 1
}

/*** MANAGE TIMEOUT ***/

// getTimeOut returns the timeout duration depending on the node state
func (node *SchedulerNode) getTimeOut() time.Duration {
	switch node.State {
	case core.FollowerState:
		return node.ElectionTimeout
	case core.CandidateState:
		return node.ElectionTimeout
	case core.LeaderState:
		return core.Config.IsAliveNotificationInterval
	}
	logger.Panic("Invalid node state", zap.String("Node", node.Card.String()), zap.Int("state", int(node.State)))
	panic("Invalid node state")
}

// handleTimeout handles the timeout event
func (node *SchedulerNode) handleTimeout() {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore timeout", zap.String("Node", node.Card.String()))
		return
	}
	switch node.State {
	case core.FollowerState:
		logger.Warn("Leader does not respond", zap.String("Node", node.Card.String()), zap.Duration("electionTimeout", node.ElectionTimeout))
		node.startNewElection()
	case core.CandidateState:
		logger.Warn("Too much time to get a majority vote", zap.String("Node", node.Card.String()), zap.Duration("electionTimeout", node.ElectionTimeout))
		node.startNewElection()
	case core.LeaderState:
		logger.Info("It's time for the Leader to send an IsAlive notification to followers", zap.String("Node", node.Card.String()))
		node.broadcastSynchronizeCommandRPC()
	}
}

/*** BROADCASTING ***/

// broadcastRequestVote broadcasts a RequestVote RPC to all the nodes (except itself)
func (node *SchedulerNode) broadcastRequestVote() {
	for i := uint32(0); i < core.Config.SchedulerNodeCount; i++ {
		if i != node.Id {
			lastLogIndex := uint32(len(node.log))
			channel := core.Config.NodeChannelMap[core.SchedulerNodeType][i].RequestVote
			request := core.RequestVoteRPC{
				FromNode:     node.Card,
				ToNode:       core.NodeCard{Id: i, Type: core.SchedulerNodeType},
				Term:         node.CurrentTerm,
				CandidateId:  node.Id,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  node.LogTerm(lastLogIndex),
			}
			channel <- request
		}
	}
}

// sendSynchronizeCommandRPC sends a SynchronizeCommand RPC to a node
func (node *SchedulerNode) sendSynchronizeCommandRPC(nodeId uint32) {
	channel := core.Config.NodeChannelMap[core.SchedulerNodeType][nodeId].RequestCommand
	lastIndex := uint32(len(node.log))

	request := core.RequestCommandRPC{
		FromNode:    node.Card,
		ToNode:      core.NodeCard{Id: nodeId, Type: core.SchedulerNodeType},
		CommandType: core.SynchronizeCommand,

		Term:        node.CurrentTerm,
		PrevIndex:   node.nextIndex[nodeId] - 1,
		PrevTerm:    node.LogTerm(node.nextIndex[nodeId] - 1),
		Entries:     core.ExtractListFromMap(&node.log, node.nextIndex[nodeId], lastIndex),
		CommitIndex: node.commitIndex,
	}

	channel <- request
}

// brodcastSynchronizeCommand sends a SynchronizeCommand to all nodes (except itself)
func (node *SchedulerNode) broadcastSynchronizeCommandRPC() {
	for i := uint32(0); i < core.Config.SchedulerNodeCount; i++ {
		if i != node.Id {
			node.sendSynchronizeCommandRPC(i)
		}
	}
}

/*** CONFIGURATION COMMAND ***/

// startElection starts an new election in sending a RequestVote RPC to all the nodes
func (node *SchedulerNode) startNewElection() {
	logger.Info("Start new election", zap.String("Node", node.Card.String()))
	node.State = core.CandidateState
	node.VoteCount = 1
	node.CurrentTerm++
	node.VotedFor = int32(node.Id)
	node.broadcastRequestVote()
}

// handleStartCommand starts the node when it receives a StartCommand
func (node *SchedulerNode) handleStartCommand() {
	if node.IsStarted {
		logger.Debug("Node already started",
			zap.String("Node", node.Card.String()),
		)
		return
	} else {
		node.IsStarted = true
	}
}

// handleCrashCommand crashes the node when it receives a CrashCommand
func (node *SchedulerNode) handleCrashCommand() {
	if node.IsCrashed {
		logger.Debug("Node is already crashed",
			zap.String("Node", node.Card.String()),
		)
		return
	} else {
		node.IsCrashed = true
	}
}

// handleRecoversCommand recovers the node after crash when it receives a RecoverCommand
func (node *SchedulerNode) handleRecoverCommand() {
	if node.IsCrashed {
		node.IsCrashed = false
	} else {
		logger.Debug("Node is not crashed",
			zap.String("Node", node.Card.String()),
		)
		return
	}
}

/*** HANDLE RPC ***/

// handleRequestSynchronizeCommand handles the SynchronizeCommand to synchronize the entries and check if the leader is alive
func (node *SchedulerNode) handleRequestSynchronizeCommand(request core.RequestCommandRPC) {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore synchronize command",
			zap.String("Node", node.Card.String()),
		)
		return
	}

	response := core.ResponseCommandRPC{
		FromNode:    node.Card,
		ToNode:      request.FromNode,
		Term:        node.CurrentTerm,
		CommandType: request.CommandType,
	}
	channel := core.Config.NodeChannelMap[request.FromNode.Type][request.FromNode.Id].ResponseCommand

	node.updateTerm(request.Term)

	// Si request term < current term (sync pas à jours), alors on ignore et on répond false
	if node.CurrentTerm > request.Term {
		logger.Debug("Ignore synchronize command because request term < current term",
			zap.String("Node", node.Card.String()),
			zap.Uint32("request term", request.Term),
			zap.Uint32("current term", node.CurrentTerm),
		)
		response.Success = false
		channel <- response
		return
	}

	// Seul le leader peut envoyer des commandes Sync donc on met à jour leaderId
	node.LeaderId = int(request.FromNode.Id)

	if node.State != core.FollowerState {
		logger.Info("Node become Follower",
			zap.String("Node", node.Card.String()),
		)
		node.State = core.FollowerState
	}

	lastLogConsistency := node.LogTerm(request.PrevIndex) == request.PrevTerm &&
		request.PrevIndex <= uint32(len(node.log))
	success := request.PrevIndex == 0 || lastLogConsistency
	logger.Debug("Fields of received synchronization request",
		zap.String("Node", node.Card.String()),
		zap.Bool("lastLogConsistency", lastLogConsistency),
		zap.Uint32("request.PrevIndex", request.PrevIndex),
		zap.Uint32("len(node.log)", uint32(len(node.log))),
		zap.Uint32("request.PrevTerm", request.PrevTerm),
		zap.Uint32("node.LogTerm(request.PrevIndex)", node.LogTerm(request.PrevIndex)),
		zap.String("request.entries", fmt.Sprintf("%v", request.Entries)),
	)

	var index uint32
	if success {
		index = request.PrevIndex
		for j := 0; j < len(request.Entries); j++ {
			index++
			if node.LogTerm(index) != request.Entries[j].Term {
				node.log[index] = request.Entries[j]
			}
		}
		core.FlushAfterIndex(&node.log, index)
		node.commitIndex = utils.MinUint32(request.CommitIndex, index)
	} else {
		index = 0
	}

	response.MatchIndex = index
	response.Success = success
	channel <- response
}

// handleResponseSynchronizeCommand handles the response of the SynchronizeCommand
func (node *SchedulerNode) handleResponseSynchronizeCommand(response core.ResponseCommandRPC) {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore synchronize command",
			zap.String("Node", node.Card.String()),
		)
		return
	}

	logger.Debug("Receive synchronize command response",
		zap.String("FromNode", response.FromNode.String()),
		zap.String("ToNode", node.Card.String()),
		zap.Bool("Success", response.Success),
		zap.Uint32("MatchIndex", response.MatchIndex),
	)

	node.updateTerm(response.Term)
	if node.State == core.LeaderState && node.CurrentTerm == response.Term {
		fromNode := response.FromNode.Id
		if response.Success {
			node.matchIndex[fromNode] = response.MatchIndex
			node.nextIndex[fromNode] = response.MatchIndex + 1
		} else {
			node.nextIndex[fromNode] = utils.MaxUint32(1, node.nextIndex[fromNode]-1)
		}
	}
}

// handleAppendEntryCommand handles the AppendEntryCommand sent to the leader to append an entry to the log and ignore the command if the node is not the leader
func (node *SchedulerNode) handleAppendEntryCommand(request core.RequestCommandRPC) {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore AppendEntry command",
			zap.String("Node", node.Card.String()),
		)
		return
	}

	channel := core.Config.NodeChannelMap[request.FromNode.Type][request.FromNode.Id].ResponseCommand
	response := core.ResponseCommandRPC{
		FromNode:    node.Card,
		ToNode:      request.FromNode,
		Term:        node.CurrentTerm,
		CommandType: request.CommandType,
		LeaderId:    node.LeaderId,
	}

	if node.State == core.LeaderState {
		entry := request.Entries[0] // Append only one entry at a time

		logger.Info("I am the leader ! Submit Job.... ",
			zap.String("Node", node.Card.String()),
			zap.String("JobRef", entry.Job.GetReference()),
		)

		entry.Term = node.CurrentTerm
		entry.WorkerId = int(node.GetWorkerId())
		entry.Job.Id = node.GetJobId()
		entry.Job.Term = node.CurrentTerm
		entry.Job.Status = core.JobWaiting
		node.addEntryToLog(entry)
		response.Success = true

	} else {
		logger.Debug("Node is not the leader. Ignore AppendEntry command and redirect to leader",
			zap.String("Node", node.Card.String()),
			zap.Int("Presumed leader id", node.LeaderId),
		)
		response.Success = false
	}

	channel <- response
}

//  handleRequestCommandRPC handles the command RPC sent to the node
func (node *SchedulerNode) handleRequestCommandRPC(request core.RequestCommandRPC) {
	logger.Debug("Handle Request Command RPC",
		zap.String("FromNode", request.FromNode.String()),
		zap.String("ToNode", request.ToNode.String()),
		zap.String("CommandType", request.CommandType.String()),
	)
	switch request.CommandType {
	case core.SynchronizeCommand:
		node.handleRequestSynchronizeCommand(request)
	case core.AppendEntryCommand:
		node.handleAppendEntryCommand(request)
	case core.StartCommand:
		node.handleStartCommand()
	case core.CrashCommand:
		node.handleCrashCommand()
	case core.RecoverCommand:
		node.handleRecoverCommand()
	}
}

// handleResponseCommandRPC handles the response command RPC sent to the node
func (node *SchedulerNode) handleResponseCommandRPC(response core.ResponseCommandRPC) {
	logger.Debug("Handle Response Command RPC",
		zap.String("FromNode", response.FromNode.String()),
		zap.String("ToNode", response.ToNode.String()),
		zap.Uint32("term", response.Term),
		zap.Bool("success", response.Success),
	)
	switch response.CommandType {
	case core.SynchronizeCommand:
		node.handleResponseSynchronizeCommand(response)
	default:
		logger.Error("Unknown response command type",
			zap.String("CommandType", response.CommandType.String()),
		)
	}
}

// handleRequestVoteRPC handles the request vote RPC sent to the node
func (node *SchedulerNode) handleRequestVoteRPC(request core.RequestVoteRPC) {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore request vote RPC",
			zap.String("FromNode", request.FromNode.String()),
			zap.String("ToNode", request.ToNode.String()),
		)
		return
	}
	logger.Debug("Handle Request Vote RPC",
		zap.String("FromNode", request.FromNode.String()),
		zap.String("ToNode", request.ToNode.String()),
		zap.Int("CandidateId", int(request.CandidateId)),
	)

	node.updateTerm(request.Term)

	channel := core.Config.NodeChannelMap[core.SchedulerNodeType][request.FromNode.Id].ResponseVote
	response := core.ResponseVoteRPC{
		FromNode:    request.ToNode,
		ToNode:      request.FromNode,
		Term:        node.CurrentTerm,
		VoteGranted: false,
	}

	lastLogIndex := uint32(len(node.log))
	lastLogTerm := node.LogTerm(lastLogIndex)
	logConsistency := request.LastLogTerm > lastLogTerm ||
		(request.LastLogTerm == lastLogTerm && request.LastLogIndex >= lastLogIndex)

	if node.CurrentTerm == request.Term &&
		node.checkVote(request.CandidateId) &&
		logConsistency {

		logger.Debug("Vote granted !",
			zap.String("Node", node.Card.String()),
			zap.Uint32("CandidateId", request.CandidateId),
		)
		node.VotedFor = int32(request.CandidateId)
		response.VoteGranted = true
	} else {
		logger.Debug("Vote refused !",
			zap.String("Node", node.Card.String()),
			zap.Uint32("CandidateId", request.CandidateId),
		)
		response.VoteGranted = false
	}
	channel <- response
}

// handleResponseVoteRPC handles the response vote RPC sent to the node
func (node *SchedulerNode) handleResponseVoteRPC(response core.ResponseVoteRPC) {
	if node.IsCrashed {
		logger.Debug("Node is crashed. Ignore request vote RPC",
			zap.String("FromNode", response.FromNode.String()),
			zap.String("ToNode", response.ToNode.String()),
		)
		return
	}

	logger.Debug("Handle Response Vote RPC",
		zap.String("FromNode", response.FromNode.String()),
		zap.String("ToNode", response.ToNode.String()),
		zap.Int("CandidateId", int(response.ToNode.Id)),
	)

	node.updateTerm(response.Term)
	if node.State == core.CandidateState &&
		node.CurrentTerm == response.Term {

		if response.VoteGranted {
			node.VoteCount++

			// When a candidate wins an election, it becomes leader.
			if node.VoteCount > core.Config.SchedulerNodeCount/2 {
				node.becomeLeader()
				return
			}
		}
	} else {
		logger.Debug("Node is not a candidate. Ignore response vote RPC",
			zap.String("Node", node.Card.String()),
			zap.String("VoteFromNode", response.FromNode.String()),
			zap.String("state", node.State.String()),
		)
	}
}

// becomeLeader sets the node as leader
func (node *SchedulerNode) becomeLeader() {
	node.State = core.LeaderState
	node.LeaderId = int(node.Card.Id)
	logger.Info("Leader elected", zap.String("Node", node.Card.String()))
	for nodeId := uint32(0); nodeId < core.Config.SchedulerNodeCount; nodeId++ {
		node.nextIndex[nodeId] = uint32(len(node.log)) + 1
	}
	node.jobIdCounter = 0
}

// updateTerm updates the term of the node if the term is higher than the current term
func (node *SchedulerNode) updateTerm(term uint32) {
	if term > node.CurrentTerm {
		logger.Debug("Update term, reset vote and change state to follower",
			zap.String("Node", node.Card.String()),
			zap.Uint32("CurrentTerm", node.CurrentTerm),
			zap.Uint32("NewTerm", term),
			zap.String("OldState", node.State.String()),
		)
		node.CurrentTerm = term
		node.State = core.FollowerState
		node.VotedFor = core.NO_NODE
	}
}

// checkVote checks if the node has already voted for the candidate
func (node *SchedulerNode) checkVote(candidateId uint32) bool {
	if node.VotedFor == core.NO_NODE || uint32(node.VotedFor) == candidateId {
		return true
	}
	return false
}

// LogTerm returns the term of the log entry at index i, or 0 if no such entry exists
func (node *SchedulerNode) LogTerm(i uint32) uint32 {
	if i < 1 || i > uint32(len(node.log)) {
		return 0
	}
	return node.log[i].Term
}

// updateCommitIndex updates the commit index of the node
func (node *SchedulerNode) updateCommitIndex() {
	if node.State != core.LeaderState {
		return
	}

	// Find the largest number M such that a majority of nodes has matchIndex[i] ≥ M
	matchIndexMedianList := make([]uint32, len(node.matchIndex)+1)
	copy(matchIndexMedianList, node.matchIndex)
	matchIndexMedianList = append(matchIndexMedianList, uint32(len(node.log)))
	sort.Slice(matchIndexMedianList, func(i, j int) bool { return matchIndexMedianList[i] < matchIndexMedianList[j] })
	median := matchIndexMedianList[core.Config.SchedulerNodeCount/2]

	if node.LogTerm(median) == node.CurrentTerm {
		node.commitIndex = median
	}
}

// GetJobId generates a new job id and increments the job id counter
func (node *SchedulerNode) GetJobId() uint32 {
	node.jobIdCounter++
	return node.jobIdCounter
}

// GetWorkerId finds the appropriate worker id for the job (the worker with the lowest load)
func (node *SchedulerNode) GetWorkerId() uint32 {
	// the number of jobs in the queue for each worker
	jobCount := make([]uint32, core.Config.WorkerNodeCount)
	for i, container := range core.Config.NodeChannelMap[core.WorkerNodeType] {
		jobCount[i] = uint32(len(container.JobQueue))
	}
	// get the worker id with the lowest number of jobs in the queue
	return utils.IndexMinUint32(jobCount)
}
