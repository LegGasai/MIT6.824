package kvraft

import (
	"6.824/labgob"
	"6.824/labrpc"
	"6.824/raft"
	"bytes"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const Debug = false
const TIMEOUT = 250
const SnapInterval= 10

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

type OpType string

const (
	GET OpType	="Get"
	PUT			="Put"
	APPEND  	="Append"
)

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Key   		string
	Value 		string
	Type    	OpType
	ClientId	int64
	CommandId	int64
}


type KVStateMachine struct {
	KVData 	map[string]string
}

func (stateMachine *KVStateMachine) Get(key string)  (Err,string) {
	value,isExist:=stateMachine.KVData[key]
	if isExist{
		return OK,value
	}else{
		return ErrNoKey,""
	}
}

func (stateMachine *KVStateMachine) Put(key string,value string)  Err {
	stateMachine.KVData[key] = value
	return OK
}

func (stateMachine *KVStateMachine) Append(key string,value string)  Err {
	stateMachine.KVData[key] += value
	return OK
}


type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	stateMachine    KVStateMachine
	waitChMap 		map[int]chan CommandReply
	cacheMap		map[int64]int64
	lastApplied		int
	lastSnapshot	int
	snapshotCond 	*sync.Cond
}

func (kv *KVServer) applyToStateMachine(command Op) CommandReply{
	if command.Type == GET{
		err,res := kv.stateMachine.Get(command.Key)
		return CommandReply{
			Err: err,
			Value: res,
		}
	}else if command.Type == PUT{
		err := kv.stateMachine.Put(command.Key,command.Value)
		return CommandReply{
			Err: err,
		}
	}else if command.Type == APPEND{
		err := kv.stateMachine.Append(command.Key,command.Value)
		return CommandReply{
			Err: err,
		}
	}
	return CommandReply{}
}

func (kv *KVServer) Command(args *CommandArgs,reply *CommandReply) {
	kv.mu.Lock()
	// replicate?
	if args.Type!=GET && kv.hasCache(args.ClientId,args.CommandId){
		reply.Err = OK
		DPrintf("[Duplicate Request][Command()]: Server[%d] received a duplicated request:[%v] and return cache | %s\n",kv.me,args,time.Now().Format("15:04:05.000"))
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()
	// leader?
	comm := Op{
		Key: args.Key,
		Value: args.Value,
		Type: args.Type,
		ClientId: args.ClientId,
		CommandId: args.CommandId,
	}
	index, _, isLeader:=kv.rf.Start(comm)
	if !isLeader{
		reply.Err = ErrWrongLeader
		DPrintf("[Not Leader][Command()]: Server[%d] is not a leader and return | %s\n",kv.me,time.Now().Format("15:04:05.000"))
		return
	}
	// wait for applyCh
	kv.mu.Lock()
	ch:=kv.getWaitCh(index)
	kv.mu.Unlock()
	DPrintf("[Wait Raft][Command()]: Server[%d] start a command:[%v] and wait raft | %s\n",kv.me,comm,time.Now().Format("15:04:05.000"))

	select {
	case res:=<-ch:
		reply.Err,reply.Value = res.Err,res.Value
		DPrintf("[Command Success][Command()]: Server[%d] has reply a request[%d] from client[%v] and reply:[%v] | %s\n",kv.me,comm.ClientId,comm.CommandId,res,time.Now().Format("15:04:05.000"))
	case <-time.After(TIMEOUT*time.Millisecond):
		reply.Err = ErrTimeout
		DPrintf("[Command Timeout][Command()]: Server[%d] timeout to reply for request[%d] from client[%v] | %s\n",kv.me,comm.ClientId,comm.CommandId,time.Now().Format("15:04:05.000"))
	}

	go kv.clearWaitCh(index)

}

func (kv *KVServer) getWaitCh(index int) chan CommandReply {
	ch,ok := kv.waitChMap[index]
	if !ok{
		ch = make(chan CommandReply,1)
		kv.waitChMap[index] = ch
	}
	return ch
}

func (kv *KVServer) clearWaitCh(index int)  {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.waitChMap,index)
	DPrintf("[Delete Chan][clearWaitCh()]: Server[%d] has deleted chan with index[%d] | %s\n",kv.me,index,time.Now().Format("15:04:05.000"))

}

func (kv *KVServer) hasCache(clientId int64,commandId int64) bool {
	item,ok := kv.cacheMap[clientId]
	if ok{
		return item>=commandId
	}else{
		return false
	}
}

func (kv *KVServer) isNeedSnapshot() bool{
	//goroutine to notify raft to snapshot
	if kv.maxraftstate != -1 && kv.rf.RaftStateSize() > kv.maxraftstate{
		DPrintf("[Need Snapshot][isNeedSnapshot()]:Server[%d] log size:[%d] and need a snapshot | %s\n",kv.me,kv.rf.RaftStateSize(),time.Now().Format("15:04:05.000"))
		return true
	}
	return false
}
// @Deprecated
func (kv *KVServer) snapshot() {
	//goroutine to notify raft to snapshot
	for !kv.killed(){

		kv.mu.Lock()
		if kv.isNeedSnapshot() && kv.lastApplied > kv.lastSnapshot{
			kv.sendSnapshot(kv.lastApplied)
		}else{
			kv.snapshotCond.Wait()
		}
		kv.mu.Unlock()
		//time.Sleep(SnapInterval*time.Millisecond)
	}
}

func (kv *KVServer) sendSnapshot(index int){
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	if e.Encode(kv.stateMachine) != nil ||
		e.Encode(kv.cacheMap) != nil {
	}
	DPrintf("[Snapshot Send][sendSnapshot()] Server[%d] ask its raft to snapshot with index[%d] | %s\n",kv.me,index, time.Now().Format("15:04:05.000"))
	kv.rf.Snapshot(index, w.Bytes())
	kv.lastSnapshot = kv.lastApplied
}

func (kv *KVServer) setSnapshot(snapshot []byte){
	if snapshot == nil || len(snapshot) < 1 { // bootstrap without any state?
		return
	}
	r := bytes.NewBuffer(snapshot)
	d := labgob.NewDecoder(r)
	var stateMachine KVStateMachine
	var cacheMap map[int64]int64
	if d.Decode(&stateMachine) != nil || d.Decode(&cacheMap) != nil {
		DPrintf("[Restore Error][setSnapshot()] Restore fail from persisted state! | %s\n", time.Now().Format("15:04:05.000"))
	}else {
		kv.stateMachine = stateMachine
		kv.cacheMap = cacheMap
		DPrintf("[Restore Success][setSnapshot()] Restore success from persisted state! | %s\n", time.Now().Format("15:04:05.000"))
	}
}

func (kv *KVServer) applier() {
	//goroutine to receive comand from raft and apply to state machine
	for !kv.killed(){

		select {
			case msg:=<-kv.applyCh:
				if msg.SnapshotValid{
					// apply snapshot
					kv.mu.Lock()
					if kv.rf.CondInstallSnapshot(msg.SnapshotTerm,msg.SnapshotIndex,msg.Snapshot){
						DPrintf("[Snapshot Msg][applier()]: Server[%d] receive Snapshot message with shapshotIndex[%d] and update its lastApplied | %s\n",kv.me,msg.SnapshotIndex,time.Now().Format("15:04:05.000"))
						kv.setSnapshot(msg.Snapshot)
						kv.lastApplied = msg.SnapshotIndex
					}
					kv.mu.Unlock()
				}else if msg.CommandValid{
					// apply to state machine
					kv.mu.Lock()
					// outdated command
					if msg.CommandIndex<=kv.lastApplied{
						DPrintf("[Outdated Msg][applier()]: Server[%d] discards outdated message with index[%d],lastApplied[%d] | %s\n",kv.me,msg.CommandIndex,kv.lastApplied,time.Now().Format("15:04:05.000"))
						kv.mu.Unlock()
						continue
					}

					kv.lastApplied = msg.CommandIndex
					command:=msg.Command.(Op)

					var commandReply CommandReply
					if command.Type!=GET && kv.hasCache(command.ClientId,command.CommandId){
						DPrintf("[Duplicate Msg][applier()]: Server[%d] find a duplicated message clientId:[%d] commandId:[%d] | %s\n",kv.me,command.ClientId,command.CommandId,time.Now().Format("15:04:05.000"))
						commandReply.Err=OK
					}else{
						commandReply = kv.applyToStateMachine(command)
						if command.Type!=GET{
							kv.cacheMap[command.ClientId]=command.CommandId
						}
						DPrintf("[Apply Msg][applier()]: Server[%d] apply a command to state machine command:[%v] | %s\n",kv.me,command,time.Now().Format("15:04:05.000"))
						//fmt.Printf("[Apply Msg][applier()]: Server[%d] apply a command to state machine command:[%d] | %s\n",kv.me,msg.CommandIndex,time.Now().Format("15:04:05.000"))
					}

					// if leader
					currentTerm,isLeader:=kv.rf.GetState()
					DPrintf("[DEBUG][applier()]: Server[%d] in term[%d] and isLeader[%t] msg:[%d] | %s\n",kv.me,currentTerm,isLeader,msg.CommandTerm,time.Now().Format("15:04:05.000"))
					if isLeader && currentTerm == msg.CommandTerm {
						ch := kv.getWaitCh(msg.CommandIndex)
						ch<-commandReply
					}
					//fmt.Printf("[%d] : [%d] : [%d]\n",kv.me,kv.lastApplied,kv.lastSnapshot)
					if kv.isNeedSnapshot() && kv.lastApplied > kv.lastSnapshot{
						kv.sendSnapshot(kv.lastApplied)
					}
					//kv.snapshotCond.Broadcast()
					kv.mu.Unlock()
				}

		}

	}
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
}


//
// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
//
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	// You may need initialization code here.
	kv.stateMachine = KVStateMachine{KVData: make(map[string]string)}
	kv.waitChMap = make(map[int]chan CommandReply)
	kv.cacheMap = make(map[int64]int64)
	kv.lastApplied = 0
	kv.snapshotCond = sync.NewCond(&kv.mu)

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.setSnapshot(persister.ReadSnapshot())
	// You may need initialization code here.
	go kv.applier()
	// go kv.snapshot()
	return kv
}
