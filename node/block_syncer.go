/**
  @author: decision
  @date: 2023/3/13
  @note: 同步器，在完成同步之前管理所有的对端节点，进行区块数据的同步，在完成同步后才将管理权交给 backend
**/

package node

import (
	"github.com/chain-lab/go-chronos/common"
	"github.com/chain-lab/go-chronos/core"
	"github.com/chain-lab/go-chronos/metrics"
	"github.com/chain-lab/go-chronos/p2p"
	"github.com/libp2p/go-libp2p/core/peer"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

const (
	maxBufferSize = 12
)

const (
	syncPaused    uint8 = 0x00 // 同步状态为暂停
	blockSyncing  uint8 = 0x01 // 同步区块中，此时关闭缓冲区的接收
	bufferSyncing uint8 = 0x02 // 同步缓冲区，开放缓冲区的接收
	synced        uint8 = 0x03 // 同步完成，到达缓冲高度，加入共识
)

const (
	blockNoneMark      uint8 = 0x00 // 已发送区块请求，但是还没有回复
	blockGottenMark    uint8 = 0x01 // 已经得到区块
	blockRequestedMark uint8 = 0x02 // 没有发送区块的请求
)

const (
	checkInterval        = 100 * time.Millisecond
	requestBlockInterval = 3 * time.Second
)

type BlockSyncerConfig struct {
	Chain *core.BlockChain
}

type BlockSyncer struct {
	peerSet []*Peer

	remoteHeight int64 // 目前其他节点的最高高度
	targetHeight int64 // 到达 buffer 同步状态后的目标高度
	knownHeight  int64 // 目前已知节点中的最新高度
	//dbLatestHeight   int64               // 数据库中存储的最高区块高度
	requestTimestamp map[int64]time.Time     // 区块请求的时间戳, height -> time
	blockMap         map[int64]*common.Block // height -> block 对应高度的区块
	peerReqTime      map[peer.ID]time.Time

	chain          *core.BlockChain
	status         uint8
	statusMsg      chan *p2p.SyncStatusMsg
	lock           sync.RWMutex
	peerStatusLock sync.RWMutex

	//cond *sync.Cond
}

func NewBlockSyncer(config *BlockSyncerConfig) *BlockSyncer {
	syncer := BlockSyncer{
		peerSet: make([]*Peer, 0, 40),

		remoteHeight:     -3,
		targetHeight:     -2,
		knownHeight:      -1,
		requestTimestamp: make(map[int64]time.Time),
		blockMap:         make(map[int64]*common.Block),
		peerReqTime:      make(map[peer.ID]time.Time),

		chain:     config.Chain,
		status:    syncPaused,
		statusMsg: make(chan *p2p.SyncStatusMsg, maxSyncerStatusChannel),
	}
	metrics.BlockSyncerStatusSet(int8(syncPaused))

	return &syncer
}

func (bs *BlockSyncer) Start() {
	metrics.RoutineCreateCounterObserve(11)
	go bs.run()
	go bs.statusMsgRoutine()
	go bs.blockProcessRoutine()

	bs.lock.Lock()
	metrics.BlockSyncerStatusSet(int8(blockSyncing))
	bs.status = blockSyncing
	bs.lock.Unlock()
	log.Infoln("Start process block syncer.")
}

// Run 同步协程，每秒触发检查是否有空闲的 peer，如果有，就由该 peer 去拉取区块
func (bs *BlockSyncer) run() {
	ticker := time.NewTicker(500 * time.Millisecond)
	log.Traceln("Start block syncer routine.")
	for {
		select {
		// 每隔 100 ms 检查一次是否存在空闲的 peer，如果有则进行区块的拉取
		case <-ticker.C:
			available := make([]*Peer, 0, len(bs.peerSet))
			bs.peerStatusLock.Lock()
			for idx := range bs.peerSet {
				p := bs.peerSet[idx]

				// todo: 这里目前不考虑大量节点的情况， 后续需要对这部分内存进行回收
				if p.Stopped() {
					continue
				}
				available = append(available, p)

				id := p.peerID
				if p.MarkSynced() || time.Since(bs.
					peerReqTime[id]) < requestBlockInterval {
					log.Traceln("Peer just send msg, loop continue.")
					continue
				}

				height := bs.selectBlockHeight()
				if height < 0 {
					continue
				}
				log.Infof("Select to get block #%d", height)

				metrics.RoutineCreateCounterObserve(14)
				go requestSyncGetBlock(height, p)
				p.SetMarkSynced(true)
			}
			bs.peerSet = available
			bs.peerStatusLock.Unlock()
		}

		// 关闭，不再主动拉取
		bs.lock.RLock()
		if bs.status == synced {
			bs.lock.RUnlock()
			log.Infoln("Block syncer exit.")
			break
		}
		bs.lock.RUnlock()
	}
}

// statusMsgRoutine 状态信息处理协程，获取状态信息队列中的信息， 然后计算对端最高高度
func (bs *BlockSyncer) statusMsgRoutine() {
	for {
		select {
		case msg := <-bs.statusMsg:
			height := msg.LatestHeight
			bufferHeight := msg.BufferedEndHeight
			bs.lock.Lock()

			if height == -1 {
				bs.lock.Unlock()
				continue
			}

			bs.remoteHeight = max(height, bs.remoteHeight)
			log.WithField("Remote height", height).Traceln("Sync remote height.")

			if bufferHeight != -1 && bs.status == blockSyncing {
				bs.targetHeight = max(bufferHeight, bs.targetHeight)
				log.WithField("Buffer height", bufferHeight).Traceln("Sync buffer height.")
			}

			dbLatestHeight := bs.chain.Height()

			if bs.remoteHeight == bs.knownHeight {
				//log.WithFields(log.Fields{
				//	"remote": bs.remoteHeight,
				//	"local":  bs.knownHeight,
				//}).Infoln("Reach remote block height.")
				metrics.BlockSyncerStatusSet(int8(bufferSyncing))
				bs.status = bufferSyncing
			}

			if dbLatestHeight == bs.targetHeight {
				log.Infoln("Reach target block height.")
				// 非创世区块节点在这里才到达同步完成的状态
				//go func() {
				//	// todo: 处理报错
				//	block, _ := bs.chain.GetLatestBlock()
				//	bytesParams := block.Header.Params
				//	params, _ := utils.DeserializeGeneralParams(bytesParams)
				//
				//	seed := new(big.Int)
				//	pi := new(big.Int)
				//
				//	seed.SetBytes(params.Result)
				//	pi.SetBytes(params.Proof)
				//
				//	calculator := crypto.GetCalculatorInstance()
				//	calculator.AppendNewSeed(seed, pi)
				//}()

				metrics.BlockSyncerStatusSet(int8(synced))
				bs.status = synced
			}

			bs.lock.Unlock()
		}

		bs.lock.RLock()
		if bs.status == synced {
			bs.lock.RUnlock()
			break
		}
		bs.lock.RUnlock()
	}
}

// blockProcessRoutine 区块处理协程， 每 1s 取出map中的区块加入到 chain 中
func (bs *BlockSyncer) blockProcessRoutine() {
	ticker := time.NewTicker(checkInterval)
	for {
		select {
		case <-ticker.C:
			bs.lock.RLock()
			knownHeight := bs.knownHeight + 1
			remoteHeight := bs.remoteHeight
			bs.lock.RUnlock()

			for height := knownHeight; height <= remoteHeight; height++ {
				if bs.blockMap[height] != nil {
					//log.WithFields(log.Fields{
					//	"height": height,
					//	"target": bs.targetHeight,
					//	"remote": bs.remoteHeight,
					//	"known":  bs.knownHeight,
					//}).Info("Append block to buffer.")
					bs.insertBlock(height)
				} else {
					break
				}
			}
		}

		bs.lock.RLock()
		if bs.status == synced {
			log.Infoln("Block sync finished, exit block process routine.")
			bs.lock.RUnlock()
			break
		}
		bs.lock.RUnlock()
	}
}

func (bs *BlockSyncer) AddPeer(p *Peer) {
	bs.peerStatusLock.Lock()
	defer bs.peerStatusLock.Unlock()

	bs.peerSet = append(bs.peerSet, p)
	bs.peerReqTime[p.peerID] = time.UnixMilli(0)
}

func (bs *BlockSyncer) appendStatusMsg(msg *p2p.SyncStatusMsg) {
	bs.statusMsg <- msg
}

func (bs *BlockSyncer) selectBlockHeight() int64 {
	bs.lock.RLock()
	defer bs.lock.RUnlock()

	for height := bs.knownHeight + 1; height <= bs.remoteHeight; height++ {
		if bs.requestTimestamp[height].IsZero() {
			log.Traceln("Set map time to zero")
			bs.requestTimestamp[height] = time.UnixMilli(0)
		}

		if bs.blockMap[height] == nil && time.Since(bs.
			requestTimestamp[height]) > requestBlockInterval {
			bs.requestTimestamp[height] = time.Now()
			log.WithField("height", height).Traceln("Select height to sync.")
			return height
		} else {
			log.WithField("interval", time.Since(bs.requestTimestamp[height])).Traceln("Trace interval value.")
		}
	}

	return -1
}

func (bs *BlockSyncer) appendBlock(block *common.Block) {
	bs.lock.Lock()
	defer bs.lock.Unlock()

	blockHeight := block.Header.Height
	bs.blockMap[blockHeight] = block
}

func (bs *BlockSyncer) insertBlock(height int64) {
	bs.lock.Lock()
	defer bs.lock.Unlock()

	block, _ := bs.blockMap[height]

	if block == nil {
		log.Warningln("Block in map is nil.")
		return
	}

	//bs.chain.InsertBlock(block)
	bs.chain.AppendBlockTask(block)
	bs.knownHeight = height

	delete(bs.requestTimestamp, height)
	delete(bs.blockMap, height)
}

func (bs *BlockSyncer) getStatus() uint8 {
	bs.lock.RLock()
	defer bs.lock.RUnlock()
	return bs.status
}

func (bs *BlockSyncer) setSynced() {
	bs.lock.Lock()
	defer bs.lock.Unlock()
	bs.status = synced
	metrics.BlockSyncerStatusSet(int8(synced))
}

func max(h1 int64, h2 int64) int64 {
	if h1 > h2 {
		return h1
	} else {
		return h2
	}
}
