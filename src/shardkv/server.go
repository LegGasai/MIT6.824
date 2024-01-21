package shardkv


import (
	"6.824/labrpc"
	"6.824/shardctrler"
	"bytes"
	"log"
	"time"
)
import "6.824/raft"
import "sync"
import "6.824/labgob"


func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}
const Debug = true
const configInterval = 50
const TIMEOUT = 250

type OpType string
const (
	GET OpType	="Get"
	PUT			="Put"
	APPEND  	="Append"
)

type ShardOpType string
const (
	UPDATE_CONFIG 	ShardOpType ="UpdateConfig"
	UPDATE_SHARD_STATE			="UpdateShardState"
	UPDATE_SHARD_DB 			="UpdateShardDB"
)

type ShardState string
const (
	READY ShardState ="Ready"
	WAITING			 ="Waiting"
	GC 				 ="GC"
)

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Key   		string
	Value 		string
	Shard 		int
	Type    	OpType
	ClientId	int64
	CommandId	int64
}

type ShardOp struct {
	Type		ShardOpType
	NewConfig	shardctrler.Config
	Shard		int
	NewState	ShardState
	TargetGid 	int
	Groups		[]string
	DB			map[string]string
	CacheMap	map[int64]int64
}

type ShardKV struct {
	mu           	sync.RWMutex
	me           	int
	rf           	*raft.Raft
	applyCh      	chan raft.ApplyMsg
	make_end     	func(string) *labrpc.ClientEnd
	gid          	int
	ctrlers      	[]*labrpc.ClientEnd
	maxraftstate 	int // snapshot if log grows this big

	// Your definitions here.
	mck  		 	*shardctrler.Clerk // shardctrler's client
	config 		 	shardctrler.Config // shard config
	stateMachine 	KVStateMachine 	// stateMachine
	waitChMap 		map[int]chan CommandReply
	cacheMap		map[int64]int64
	pullGid			map[int]int //Shard -> Gid
	pullGroups		map[int][]string // Gid -> servers
	lastApplied		int
	lastSnapshot	int
	cond 			*sync.Cond

}
type ShardData struct {
	State	ShardState
	DB	    map[string]string
}

type KVStateMachine struct {
	KVData 	map[int]*ShardData // Shard -> ShardData
}
func (stateMachine KVStateMachine) get(shard int,key string) (Err,string){
	shardData,ok:=stateMachine.KVData[shard];
	if !ok{
		return ErrNoKey,""
	}
	if shardData.State == WAITING{
		return ErrNotReady,""
	}
	value,isExist:=stateMachine.KVData[shard].DB[key]
	if isExist{
		return OK,value
	}else{
		return ErrNoKey,""
	}
}
func (stateMachine KVStateMachine) put(shard int,key string,value string) Err{
	if _,ok:=stateMachine.KVData[shard];!ok{
		stateMachine.KVData[shard] = &ShardData{
			State: READY,
			DB: make(map[string]string),
		}
	}
	if stateMachine.KVData[shard].State == WAITING{
		return ErrNotReady
	}
	stateMachine.KVData[shard].DB[key] = value
	return OK
}
func (stateMachine KVStateMachine) append(shard int,key string,value string) Err{
	if _,ok:=stateMachine.KVData[shard];!ok{
		stateMachine.KVData[shard] = &ShardData{
			State: READY,
			DB: make(map[string]string),
		}
	}
	if stateMachine.KVData[shard].State == WAITING{
		return ErrNotReady
	}
	stateMachine.KVData[shard].DB[key] += value
	return OK
}

type CommandReply struct {
	Value 	string
	Err		Err
}
func (kv *ShardKV) applyToStateMachine(command Op) CommandReply{
	if command.Type == GET{
		err,res := kv.stateMachine.get(command.Shard,command.Key)
		return CommandReply{
			Err: err,
			Value: res,
		}
	}else if command.Type == PUT{
		err := kv.stateMachine.put(command.Shard,command.Key,command.Value)
		return CommandReply{
			Err: err,
		}
	}else if command.Type == APPEND{
		err := kv.stateMachine.append(command.Shard,command.Key,command.Value)
		return CommandReply{
			Err: err,
		}
	}
	return CommandReply{}
}

func (kv *ShardKV) getWaitCh(index int) chan CommandReply {
	ch,ok := kv.waitChMap[index]
	if !ok{
		ch = make(chan CommandReply,1)
		kv.waitChMap[index] = ch
	}
	return ch
}

func (kv *ShardKV) clearWaitCh(index int)  {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.waitChMap,index)
	//DPrintf("[Delete Chan][clearWaitCh()]: Server[%d] has deleted chan with index[%d] | %s\n",kv.me,index,time.Now().Format("15:04:05.000"))
}

func (kv *ShardKV) hasCache(clientId int64,commandId int64) bool {
	item,ok := kv.cacheMap[clientId]
	if ok{
		return item>=commandId
	}else{
		return false
	}
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	// check whether key is current server's shard
	kv.mu.Lock()
	shard,gid,checked := kv.checkKey(args.Key)
	if !checked {
		reply.Err = ErrWrongGroup
		DPrintf("[Wrong Group][Get()]: Server[%d]-[%d] received a wrong shard with args:[%v] should [%d] but current gid[%d] and return | %s\n",kv.gid,kv.me,args,gid,kv.gid,time.Now().Format("15:04:05.000"))
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()
	command := Op{
		Key: args.Key,
		Shard: shard,
		Type: GET,
		ClientId: args.ClientId,
		CommandId: args.CommandId,
	}
	index,_,isLeader :=kv.rf.Start(command)
	if !isLeader{
		reply.Err = ErrWrongLeader
		//DPrintf("[Not Leader][Get()]: Server[%d]-[%d] is not a leader and return | %s\n",kv.gid,kv.me,time.Now().Format("15:04:05.000"))
		return
	}
	kv.mu.Lock()
	ch:=kv.getWaitCh(index)
	kv.mu.Unlock()
	select {
	case res:=<-ch:
		reply.Err,reply.Value = res.Err,res.Value
		DPrintf("[Get Success][Get()]: Server[%d]-[%d] has reply a request[%d] from client[%v] and reply:[%v] | %s\n",kv.gid,kv.me,command.ClientId,command.CommandId,res,time.Now().Format("15:04:05.000"))
	case <-time.After(TIMEOUT*time.Millisecond):
		reply.Err = ErrTimeout
		DPrintf("[Get Success][Get()]: Server[%d]-[%d] timeout to reply for request[%d] from client[%v] | %s\n",kv.gid,kv.me,command.ClientId,command.CommandId,time.Now().Format("15:04:05.000"))
	}
	go kv.clearWaitCh(index)
}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	kv.mu.Lock()
	// check whether key is current server's shard
	shard,gid,checked := kv.checkKey(args.Key)
	if !checked {
		reply.Err = ErrWrongGroup
		DPrintf("[Wrong Group][PutAppend()]: Server[%d]-[%d] received a wrong shard with args:[%v] should [%d] but current gid[%d] and return | %s\n",kv.gid,kv.me,args,gid,kv.gid,time.Now().Format("15:04:05.000"))
		kv.mu.Unlock()
		return
	}
	// replicate?
	if kv.hasCache(args.ClientId,args.CommandId){
		reply.Err = OK
		DPrintf("[Duplicate Request][PutAppend()]: Server[%d]-[%d] received a duplicated request:[%v] and return cache | %s\n",kv.gid,kv.me,args,time.Now().Format("15:04:05.000"))
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()
	command := Op{
		Key: args.Key,
		Value: args.Value,
		Shard: shard,
		Type: OpType(args.Op),
		ClientId: args.ClientId,
		CommandId: args.CommandId,
	}
	index,_,isLeader :=kv.rf.Start(command)
	if !isLeader{
		reply.Err = ErrWrongLeader
		//DPrintf("[Not Leader][PutAppend()]: Server[%d]-[%d] is not a leader and return | %s\n",kv.gid,kv.me,time.Now().Format("15:04:05.000"))
		return
	}
	// wait for applyCh
	kv.mu.Lock()
	ch:=kv.getWaitCh(index)
	kv.mu.Unlock()

	select {
	case res:=<-ch:
		reply.Err = res.Err
		DPrintf("[Command Success][PutAppend()]: Server[%d]-[%d] has reply a request[%d] from client[%v] and reply:[%v] | %s\n",kv.gid,kv.me,command.ClientId,command.CommandId,res,time.Now().Format("15:04:05.000"))
	case <-time.After(TIMEOUT*time.Millisecond):
		reply.Err = ErrTimeout
		DPrintf("[Command Timeout][PutAppend()]: Server[%d]-[%d] timeout to reply for request[%d] from client[%v] | %s\n",kv.gid,kv.me,command.ClientId,command.CommandId,time.Now().Format("15:04:05.000"))
	}

	go kv.clearWaitCh(index)
}

// Snapshot service
func (kv *ShardKV) isNeedSnapshot() bool{
	//goroutine to notify raft to snapshot
	if kv.maxraftstate != -1 && kv.rf.RaftStateSize() > kv.maxraftstate{
		//DPrintf("[Need Snapshot][isNeedSnapshot()]:Server[%d]-[%d] log size:[%d] and need a snapshot | %s\n",kv.gid,kv.me,kv.rf.RaftStateSize(),time.Now().Format("15:04:05.000"))
		return true
	}
	return false
}

func (kv *ShardKV) sendSnapshot(index int){
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	if  e.Encode(kv.stateMachine) != nil ||
		e.Encode(kv.cacheMap) != nil ||
		e.Encode(kv.pullGid) != nil ||
		e.Encode(kv.pullGroups) != nil ||
		e.Encode(kv.config) != nil{
	}
	//DPrintf("[Snapshot Send][sendSnapshot()] Server[%d]-[%d] ask its raft to snapshot with index[%d] | %s\n",kv.gid,kv.me,index, time.Now().Format("15:04:05.000"))
	kv.rf.Snapshot(index, w.Bytes())
	kv.lastSnapshot = kv.lastApplied
}

func (kv *ShardKV) setSnapshot(snapshot []byte){
	if snapshot == nil || len(snapshot) < 1 { // bootstrap without any state?
		return
	}
	r := bytes.NewBuffer(snapshot)
	d := labgob.NewDecoder(r)
	var stateMachine KVStateMachine
	var cacheMap map[int64]int64
	var pullGid map[int]int
	var pullGroups map[int][]string
	var config shardctrler.Config
	if  d.Decode(&stateMachine) != nil ||
		d.Decode(&cacheMap) != nil ||
		d.Decode(&pullGid) != nil ||
		d.Decode(&pullGroups) != nil ||
		d.Decode(&config) != nil {
		//DPrintf("[Restore Error][setSnapshot()] Restore fail from persisted state! | %s\n", time.Now().Format("15:04:05.000"))
	}else {
		kv.stateMachine = stateMachine
		kv.cacheMap = cacheMap
		kv.pullGid = pullGid
		kv.pullGroups = pullGroups
		kv.config = config
		//DPrintf("[Restore Success][setSnapshot()] Restore success from persisted state! | %s\n", time.Now().Format("15:04:05.000"))
	}
}

//
// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *ShardKV) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
}

// ShardMigrationHandler
// todo
func (kv *ShardKV) ShardMigrationHandler(args *ShardMigrationArgs, reply * ShardMigrationReply){
	if _,isLeader := kv.rf.GetState();!isLeader{
		reply.Err = ErrWrongLeader
		return
	}
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	if args.ConfigNum > kv.config.Num{
		DPrintf("[Send ShardData Fail][ShardMigrationHandler] Server[%d]-[%d] send shard[%d] with config[%d] : [%d] | %s\n", kv.gid,kv.me,args.Shard,args.ConfigNum,kv.config.Num,time.Now().Format("15:04:05.000"))
		reply.Err = ErrNotReady
		return
	}
	var shard = args.Shard
	copyDB,copyCache := kv.deepCopyShardDataAndCacheMap(shard)
	reply.Data = copyDB
	reply.CacheMap = copyCache
	reply.ConfigNum = kv.config.Num
	reply.Shard = shard
	reply.Err = OK
	DPrintf("[Send ShardData Success][ShardMigrationHandler] Server[%d]-[%d] send shard[%d] with config[%d] : [%d] | %s\n", kv.gid,kv.me,args.Shard,args.ConfigNum,kv.config.Num,time.Now().Format("15:04:05.000"))
}

// deepCopyShardDataAndCacheMap
// todo
func (kv *ShardKV) deepCopyShardDataAndCacheMap(shard int) (map[string]string,map[int64]int64) {
	copyDB := make(map[string]string)
	copyCache := make(map[int64]int64)
	for k,v := range kv.cacheMap{
		copyCache[k]=v
	}
	if _,ok:= kv.stateMachine.KVData[shard];ok{
		for k,v := range kv.stateMachine.KVData[shard].DB{
			copyDB[k]=v
		}
	}
	return copyDB,copyCache
}

// goroutine to receive comand from raft and apply to state machine
// todo : update config command and update shard data command
func (kv *ShardKV) applier(){
	for {
		select {
		case msg:=<-kv.applyCh:
			if msg.SnapshotValid{
				// apply snapshot
				kv.mu.Lock()
				if kv.rf.CondInstallSnapshot(msg.SnapshotTerm,msg.SnapshotIndex,msg.Snapshot){
					//DPrintf("[Snapshot Msg][applier()]: Server[%d] receive Snapshot message with shapshotIndex[%d] and update its lastApplied | %s\n",kv.me,msg.SnapshotIndex,time.Now().Format("15:04:05.000"))
					kv.setSnapshot(msg.Snapshot)
					kv.lastApplied = msg.SnapshotIndex
				}
				kv.mu.Unlock()
			}else if msg.CommandValid{
				// apply to state machine
				kv.mu.Lock()
				// outdated command
				if msg.CommandIndex<=kv.lastApplied{
					DPrintf("[Outdated Msg][applier()]: Server[%d]-[%d] discards outdated message with index[%d],lastApplied[%d] | %s\n",kv.gid,kv.me,msg.CommandIndex,kv.lastApplied,time.Now().Format("15:04:05.000"))
					kv.mu.Unlock()
					continue
				}

				kv.lastApplied = msg.CommandIndex

				if command,ok:=msg.Command.(Op);ok{
					var commandReply CommandReply
					if command.Type!=GET && kv.hasCache(command.ClientId,command.CommandId){
						DPrintf("[Duplicate Msg][applier()]: Server[%d]-[%d] find a duplicated message clientId:[%d] commandId:[%d] | %s\n",kv.gid,kv.me,command.ClientId,command.CommandId,time.Now().Format("15:04:05.000"))
						commandReply.Err=OK
					}else{
						commandReply = kv.applyToStateMachine(command)
						if command.Type!=GET && commandReply.Err == OK{
							kv.cacheMap[command.ClientId]=command.CommandId
						}
						DPrintf("[Apply Msg][applier()]: Server[%d]-[%d] apply a command to state machine command:[%v] and reply:[%s] | %s\n",kv.gid,kv.me,command,commandReply.Err,time.Now().Format("15:04:05.000"))
					}
					// if leader
					currentTerm,isLeader:=kv.rf.GetState()
					if isLeader && currentTerm == msg.CommandTerm {
						ch := kv.getWaitCh(msg.CommandIndex)
						ch<-commandReply
					}
				}else if command,ok:=msg.Command.(ShardOp);ok{
					if command.Type == UPDATE_CONFIG{
						DPrintf("[UpdateConfig Msg][applier()]: Server[%d]-[%d] apply a command to state machine command:[%v] | %s\n",kv.gid,kv.me,command,time.Now().Format("15:04:05.000"))
						kv.config = command.NewConfig
					}else if command.Type == UPDATE_SHARD_STATE{
						DPrintf("[UpdateShardState Msg][applier()]: Server[%d]-[%d] apply a command to state machine command:[%v] | %s\n",kv.gid,kv.me,command,time.Now().Format("15:04:05.000"))
						kv.updateShardState(command.Shard,command.NewState,command.TargetGid,command.Groups)
					}else if command.Type == UPDATE_SHARD_DB{
						DPrintf("[UpdateShardDB Msg][applier()]: Server[%d]-[%d] apply a command to state machine command:[%v] | %s\n",kv.gid,kv.me,command,time.Now().Format("15:04:05.000"))
						kv.updateShardDBAndCacheMap(command.Shard,command.DB,command.CacheMap,command.TargetGid)
					}
				}

				if kv.isNeedSnapshot() && kv.lastApplied > kv.lastSnapshot{
					kv.sendSnapshot(kv.lastApplied)
				}
				kv.mu.Unlock()
			}
		}
	}
}

func (kv *ShardKV) updateShardState(shard int,newState ShardState,targetGid int,groups []string){
	if _,ok:=kv.stateMachine.KVData[shard];!ok{
		kv.stateMachine.KVData[shard] = &ShardData{
			State: READY,
			DB: make(map[string]string),
		}
	}
	kv.stateMachine.KVData[shard].State = newState
	if newState == WAITING{
		kv.pullGid[shard] = targetGid
		kv.pullGroups[targetGid] = groups
	}
	DPrintf("[UpdateShardState Success][updateShardState()]: Server[%d]-[%d] update shard[%d] with state[%s] | %s\n",kv.gid,kv.me,shard,kv.stateMachine.KVData[shard].State,time.Now().Format("15:04:05.000"))
}

func (kv *ShardKV) updateShardDBAndCacheMap(shard int,shardDB map[string]string,cache map[int64]int64,targetGid int){
	// merge DB map
	if shardData,ok:=kv.stateMachine.KVData[shard];!ok{
		kv.stateMachine.KVData[shard] = &ShardData{
			State: READY,
			DB: shardDB,
		}
	}else{
		shardData.State = READY
		for k,v := range shardDB{
			shardData.DB[k]=v
		}
	}
	// merge cacheMap
	for k,v := range cache{
		kv.cacheMap[k] = max(kv.cacheMap[k],v)
	}
	delete(kv.pullGid, shard)
	delete(kv.pullGroups,targetGid)
	DPrintf("[UpdateShardDB Success][updateShardDBAndCacheMap()]: Server[%d]-[%d] update Shard[%d] with ShardData:[%v] | %s\n",kv.gid,kv.me,shard,kv.stateMachine.KVData[shard],time.Now().Format("15:04:05.000"))
}

// goroutine to request for latest config from shardctrler every 50 ms
// todo : ok
func (kv *ShardKV) getConfig(){
	for{
		_,isLeader := kv.rf.GetState()
		if isLeader{
			kv.mu.Lock()
			newConfig := kv.mck.Query(kv.config.Num+1)
			DPrintf("[Get Config][getConfig()]:Server[%d]-[%d] got the latest config:[%v] | %s\n",kv.gid,kv.me,newConfig,time.Now().Format("15:04:05.000"))
			kv.checkConfig(newConfig)
			kv.mu.Unlock()
		}
		time.Sleep(time.Millisecond*configInterval)
	}
}

// goroutine to update shardData periodically
// todo
func (kv *ShardKV) updateShardData(){
	for{
		_,isLeader := kv.rf.GetState()
		if !isLeader {
			time.Sleep(time.Millisecond*10)
			continue
		}
		kv.mu.RLock()
		var wg sync.WaitGroup
		for shard,shardData := range kv.stateMachine.KVData{
			state := shardData.State
			if state == WAITING {
				wg.Add(1)
				targetGid := kv.pullGid[shard]
				servers := kv.pullGroups[targetGid]
				DPrintf("[Ask Shard Data][updateShardData()]:Server[%d]-[%d] ask shard:[%d] configNum:[%d] from gid[%d] and servers[%v] | %s\n",kv.gid,kv.me,shard,kv.config.Num,targetGid,servers,time.Now().Format("15:04:05.000"))
				go func(shard int,configNum int,targetGid int,servers []string) {
					defer wg.Done()
					args := ShardMigrationArgs{
						Shard: shard,
						ConfigNum: configNum,
					}
					for _,server := range servers{
						reply := ShardMigrationReply{}
						srv := kv.make_end(server)
						if ok:=srv.Call("ShardKV.ShardMigrationHandler",&args,&reply);ok&&reply.Err == OK{
							kv.rf.Start(ShardOp{
								Type: UPDATE_SHARD_DB,
								Shard: reply.Shard,
								DB: reply.Data,
								CacheMap: reply.CacheMap,
							})
							DPrintf("[ShardData][updateShardData()]: Server[%d]-[%d] commit a new shard data with shard[%d]  | %s\n",kv.gid,kv.me,reply.Shard,time.Now().Format("15:04:05.000"))
						}
					}
				}(shard,kv.config.Num,targetGid,servers)
			}
		}
		// wait for all shard are ready
		kv.mu.RUnlock()
		wg.Wait()
		time.Sleep(time.Millisecond*10)
	}

}


// goroutine to recycle shardData periodically
func (kv *ShardKV) GCShardData(){

}

// update config only by leader
// todo : send RPC for shard data if necessary and update config
func (kv *ShardKV) checkConfig(newConfig shardctrler.Config)  {
	oldConfig := kv.config
	// there are no newer config to update
	if newConfig.Num != oldConfig.Num+1 {
		//DPrintf("[No need to update][updateConfig]:Server[%d]-[%d] with old configNum[%d] : new configNew[%d] | %s\n",kv.gid,kv.me,kv.config.Num,newConfig.Num,time.Now().Format("15:04:05.000"))
		return
	}
	// check whether to need shard data

	// 1.update config firstly
	kv.rf.Start(ShardOp{
		Type: UPDATE_CONFIG,
		NewConfig: newConfig,
	})
	// todo check if necessary
	if oldConfig.Num == 0{
		return
	}
	// 2.check shard and update if necessary
	for index:= 0;index<len(newConfig.Shards);index++ {
		// ask other server for shard data
		if newConfig.Shards[index] == kv.gid && oldConfig.Shards[index] != kv.gid{
			// need shard data
			kv.rf.Start(ShardOp{
				Type: UPDATE_SHARD_STATE,
				Shard: index,
				NewState: WAITING,
				TargetGid: oldConfig.Shards[index],
				Groups: oldConfig.Groups[oldConfig.Shards[index]],
			})
		}else if newConfig.Shards[index] != kv.gid && oldConfig.Shards[index] == kv.gid {
			// need GC
			//kv.rf.Start(ShardOp{
			//	Type: UPDATE_SHARD_STATE,
			//	Shard: index,
			//	NewState: GC,
			//})
		}
	}
}


// Check if the shard the key belongs to is its own responsibility
func (kv *ShardKV) checkKey(key string) (int,int,bool){
	shard := key2shard(key)
	gid := kv.config.Shards[shard]
	return shard,gid,kv.config.Shards[key2shard(key)]==kv.gid
}

func max(a int64,b int64) int64{
	if a>=b {return a} else {return b}
}

//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardctrler.
//
// pass ctrlers[] to shardctrler.MakeClerk() so you can send
// RPCs to the shardctrler.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use ctrlers[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, ctrlers []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(ShardOp{})
	labgob.Register(shardctrler.Config{})
	labgob.Register(ShardMigrationArgs{})
	labgob.Register(ShardMigrationReply{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.ctrlers = ctrlers

	// Your initialization code here.

	// Use something like this to talk to the shardctrler:
	kv.mck = shardctrler.MakeClerk(kv.ctrlers)
	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.stateMachine = KVStateMachine{KVData: make(map[int]*ShardData)}
	kv.waitChMap = make(map[int]chan CommandReply)
	kv.cacheMap = make(map[int64]int64)
	kv.pullGid  = make(map[int]int)
	kv.pullGroups = make(map[int][]string)
	kv.lastApplied = 0
	kv.cond = sync.NewCond(&kv.mu)
	kv.config = shardctrler.Config{}
	kv.setSnapshot(persister.ReadSnapshot())

	go kv.getConfig()
	go kv.applier()
	go kv.updateShardData()
	return kv
}
