package raft

import (
	"sync"
	"log"
	"time"
)

type AppendEntriesArgs struct {
	Term int
	LeaderId int
	PreLogIndex int
	PreLogTerm int
	Entries []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term int
	Success bool
}

func (rf *Raft) makeHeartBeat(term int) AppendEntriesArgs {
	return AppendEntriesArgs{
		Term: term,
		LeaderId: rf.me,
		PreLogIndex: rf.getLastLogIndex(),
		PreLogTerm: rf.getLastLogTerm(),
		Entries: []LogEntry{},
		LeaderCommit: rf.commitIndex,
	}
}

func (rf *Raft) sendHeartBeats(term int) {
	log.Printf("sr %v begins send HB", rf.me)
	rf.stopChSendHB = make(chan int)
	go rf.broadCastHB(term)
	for {
		select {
		case <-rf.killedCh:
			return
		case <-rf.stopChSendHB:
			return
		case <-time.After(rf.getHBTime()):
			go rf.broadCastHB(term)
		}
	}
}

func (rf *Raft) getHBTime() time.Duration {
	return time.Millisecond*(time.Duration)(rf.heartBeatTime)
}

func (rf *Raft) broadCastHB(term int) {
	log.Printf("sr %v send HB with match %v next %v", rf.me, rf.matchIndex, rf.nextIndex)
	args := rf.makeHeartBeat(term)
	for index := range rf.peers {
		select {
		case <-rf.stopChSendHB:
			return
		default:
		}
		if index != rf.me {
			go func(index int) {
				reply := AppendEntriesReply{}
				// log.Printf("sr %v send HB to %v index", rf.me, index)
				rf.sendAppendEntries(index, &args, &reply)
				if reply.Term > term {
					rf.changeRoleCh <- follower
					rf.currentTerm = reply.Term
				}
			}(index)
		}
	}
}

// heartbeats: 100 ms
// election time elapse: 300~500 ms
func (rf *Raft) checkHeartBeats() {
	log.Printf("sr %v start checkHB", rf.me)
	rf.stopChCheckHB = make(chan int)
	for {
		select {
		case <-rf.killedCh:
			return
		case <-rf.stopChCheckHB:
			return
		case <-rf.heartBeatsCh:
			// log.Printf("sr %v get HB", rf.me)
			continue
		case <-time.After(rf.getElectionTime()):
			log.Printf("sr %v doesn't get HB", rf.me)
			if rf.state != leader {
				rf.changeRoleCh <- candidate
			}
		}
	}
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	log.Printf("sr %v term %v with log %v receive AE %v", rf.me, rf.currentTerm, rf.log, *args)

	reply.Success = false
	reply.Term = rf.currentTerm
	if args.Term < rf.currentTerm { // self is newer
		return
	}
	rf.heartBeatsCh <- 1
	if args.Term > rf.currentTerm {
		log.Printf("sr %v flag 2", rf.me)
		rf.currentTerm = args.Term
		rf.changeRoleCh <- follower
		log.Printf("sr %v flag 2.1", rf.me)
	}
	if rf.getLastLogIndex() < args.PreLogIndex {
		log.Printf("sr %v flag 3.1", rf.me)
		return
	}
	if rf.getLogByIndex(args.PreLogIndex).Term != args.PreLogTerm {
		log.Printf("sr %v flag 4", rf.me)
		rf.log = rf.getLogByIndexRange(0, args.PreLogIndex)
		log.Printf("sr %v flag 4.1", rf.me)
		return
	}
	if len(args.Entries) > 0 && len(rf.log) > args.PreLogIndex {
		log.Printf("sr %v flag 5", rf.me)
		rf.log = rf.getLogByIndexRange(0, args.PreLogIndex + 1)
		log.Printf("sr %v flag 5.1", rf.me)
	}
	log.Printf("sr %v flag 6", rf.me)
	reply.Success = true
	go func() {
		rf.log = append(rf.log, args.Entries...)
		if len(args.Entries) > 0 {
			log.Printf("sr %v log %v", rf.me, rf.log)
		}
		// log.Printf("sr %v flag 6", rf.me)
		if args.LeaderCommit > rf.commitIndex {
			// log.Printf("sr %v flag 6.1", rf.me)
			if args.LeaderCommit > rf.getLastLogIndex() {
				rf.commitIndex = rf.getLastLogIndex()
			} else {
				rf.commitIndex = args.LeaderCommit
			}
			rf.checkAppliedCh <- 1
		}
	}()
	// log.Printf("sr %v flag 7", rf.me)
	return
}

func (rf *Raft) startUpdateFollowersLog(term int) {
	rf.stopChUpdateFollowers = make(chan int)
	for index := range rf.peers {
		if index != rf.me {
			go rf.updateFollowerLog(index, term)
		}
	}
}

func (rf *Raft) updateFollowerLog(index, term int) {
	for {
		select {
		case <-rf.killedCh:
			return
		case <-rf.stopChUpdateFollowers:
			return
		case <-rf.updateFollowerLogCh[index]:
			if rf.getLastLogIndex() >= rf.nextIndex[index] {
				mu := sync.Mutex{}
				mu.Lock()
				log.Printf("leader %v next %v log %v", rf.me, rf.nextIndex, rf.log)
				log.Printf("leader %v send %v log %v", rf.me, index, rf.getLogByIndexRange(rf.nextIndex[index], -1))
				for {
					select {
					case <-rf.killedCh:
						return
					case <-rf.stopChUpdateFollowers:
						return
					default:
					}
					log.Printf("leader %v nextIndex %v index %v", rf.me, rf.nextIndex, index)
					prevLog := rf.getLogByIndex(rf.nextIndex[index]-1)
					args := AppendEntriesArgs{
						Term: term,
						LeaderId: rf.me,
						PreLogIndex: rf.nextIndex[index]-1,
						PreLogTerm: prevLog.Term,
						Entries: []LogEntry{},
						LeaderCommit: rf.commitIndex,
					}
					reply := AppendEntriesReply{}
					ok := rf.peers[index].Call("Raft.AppendEntries", &args, &reply)
					if !ok {
						log.Printf("sr %v send %v to sr %v fail", rf.me, args, index)
						log.Printf("rf.nextIndex %v", rf.nextIndex)
						continue
					}
					if reply.Success {
						break
					} else {
						if reply.Term > term {
							rf.currentTerm = reply.Term
							rf.changeRoleCh <- follower
							return
						}
						rf.nextIndex[index]--
					}
				}
				prevLog := rf.getLogByIndex(rf.nextIndex[index]-1)
				args := AppendEntriesArgs{
					Term: term,
					LeaderId: rf.me,
					PreLogIndex: rf.nextIndex[index]-1,
					PreLogTerm: prevLog.Term,
					Entries: rf.getLogByIndexRange(rf.nextIndex[index], -0),
					LeaderCommit: rf.commitIndex,
				}
				reply := AppendEntriesReply{}
				log.Printf("sr %v send update log to sr %v with %v", rf.me, index, args)
				ok := rf.peers[index].Call("Raft.AppendEntries", &args, &reply)
				if !ok {
					log.Printf("sr %v send %v to sr %v fail", rf.me, args, index)
				}
				if reply.Success {
					rf.nextIndex[index] = rf.getLastLogIndex() + 1
					rf.matchIndex[index] = rf.getLastLogIndex()
					rf.checkCommitUpdateCh <- 1
				} else {
					panic("something wrong")
				}
				mu.Unlock()
				log.Printf("sr %v update %v finish", rf.me, index)
			}
		}
	}
}

func (rf *Raft) checkCommitUpdate() {
	rf.stopChCommitUpdate = make(chan int)
	for {
		select {
		case <-rf.killedCh:
			return
		case <-rf.stopChCommitUpdate:
			return
		case <-rf.checkCommitUpdateCh:
			log.Printf("Check Commit Update with commitIndex %v, match %v", rf.commitIndex, rf.matchIndex)
			rf.mu.Lock()
			var i int
			for i = rf.getLastLogIndex(); i > rf.commitIndex; i-- {
				matches := 0
				for j := 0; j < len(rf.peers); j++ {
					if rf.matchIndex[j] >= i {
						matches++
					}
				}
				if matches > len(rf.peers)/2 && rf.getLogByIndex(i).Term == rf.currentTerm {
					break
				}
			}
			log.Printf("new commitIndex %v", i)
			rf.commitIndex = i
			go rf.broadCastHB(rf.currentTerm)
			rf.mu.Unlock()
			rf.checkAppliedCh <- 1
		}
	}
}

func (rf *Raft) triggerUpdateFollowers() {
	for i := range rf.peers {
		if i != rf.me {
			go func(i int){
				rf.updateFollowerLogCh[i] <- 1
				// log.Printf("sr %v trigger %v", rf.me, i)
			}(i)
		}
	}
}