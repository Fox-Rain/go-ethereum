// Copyright 2014 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// Package utils contains internal helper functions for go-ethereum commands.

// TODO: geth에 필요한 노드시작, 서비스등록, 설정 적용등의 핵심 유틸 함수를 담고 있다.
// 		 즉, 명령어들을 입력시 실제 동작을 만드는 "엔진함수들"이 존재하는 파일이다.
// TODO: config.go가 노드의 설정값을 구성하고 노드를 생성한다면 그 설정값에 따라 바꾸는 핵심 엔진역할의 함수는 cmd.go에 존재

/*

	| 항목           | Era1 (`ExportHistory`)                       | 일반 export (`ExportChain`, `ExportAppendChain`) |
| ------------ | -------------------------------------------- | ---------------------------------------------- |
| 📁 파일 형식     | Geth 내부용 포맷 (`.era1`)                        | RLP 스트림 (`.rlp`, `.rlp.gz`)                    |
| 📦 포함 내용     | **블록 + receipts + 누적 난이도 + Merkle root**     | **블록만** (헤더 + 트랜잭션 + uncle)                    |
| 📏 저장 단위     | `step` 크기로 블록들을 나눠 여러 파일로 저장                 | 전체 체인 혹은 지정 구간을 하나의 파일로 저장                     |
| 🧾 무결성 체크    | 각 파일마다 SHA256 체크섬 + Merkle root 포함           | 없음 (수동 검증해야 함)                                 |
| 🧬 구조화       | `era.Filename()`, `era.NewBuilder()` 등으로 구조화 | 단순한 RLP 리스트                                    |
| 📦 import 대상 | `ImportHistory()`에서만 사용 가능                   | `ImportChain()`에서 사용 가능                        |
| 🔐 보안성/무결성   | 상대적으로 더 안전하고 검증 가능                           | 구조가 단순하고, 검증은 수동이거나 insert 시 검증됨               |



* History == 제네시스부터 특정 시점까지의 전체 체인 데이터 (블록, 상태 루트, receipt, total difficulty 등 포함)




cmd.go

start node
시스템 디스크 용량 부족 감시
체인에 데이터 넣기 / 빼기 등...  + 여러 포멧 rlp, era1 ..		** 대부분 geth을 통해 터미널 커맨드로 실행되는 함수들이다.


*/

package utils

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/debug"
	"github.com/ethereum/go-ethereum/internal/era"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfave/cli/v2"
)

const (
	importBatchSize = 2500	// * 블록이나 데이터를 불러올 떄  2500단위로 배치 처리하겠다는 설정값
)

// ErrImportInterrupted is returned when the user interrupts the import process.
var ErrImportInterrupted = errors.New("interrupted")

// Fatalf formats a message to standard error and exits the program.
// The message is also printed to standard output if standard error
// is redirected to a different file.

// os에 따라 출력 대상을 정하고 포팅된 에러 메세지를 출력하는 함수
func Fatalf(format string, args ...interface{}) {
	w := io.MultiWriter(os.Stdout, os.Stderr)
	if runtime.GOOS == "windows" || runtime.GOOS == "openbsd" {
		// The SameFile check below doesn't work on Windows neither OpenBSD.
		// stdout is unlikely to get redirected though, so just print there.
		w = os.Stdout
	} else {
		outf, _ := os.Stdout.Stat()
		errf, _ := os.Stderr.Stat()
		if outf != nil && errf != nil && os.SameFile(outf, errf) {
			w = os.Stderr
		}
	}
	fmt.Fprintf(w, "Fatal: "+format+"\n", args...)
	os.Exit(1)
}



// TODO: main.go의 StartNode에서 호출되는 함수로   설정된 스택을 실제로 시작하고 인터럽트 시그널 처리, 디스크 감시, ... 등을 담당
func StartNode(ctx *cli.Context, stack *node.Node, isConsole bool) {

	// TODO: node.Node 타입인 stack을 실행 => 내부적으로 p2p, RPC, consensus등 모든 모듈을 시작한다.  **노드 내부에 모듈이 연결되어서 노드를 통해 내부적으로 시작된다.
	if err := stack.Start(); err != nil {
		Fatalf("Error starting protocol stack: %v", err)
	}

	// TODO: 고루틴을 통해 백그라운드로 종료 시그널 대기, 디스크 상태 감시, graceful shutdown을 처리한다.
	go func() {

		// OS에서 오는 종료요청을 감지하는 채널을 생성 -> 시그널 받으면 그에 따라 종료루틴 수행	 이 때문에 이 고루틴이 계속 유지된다.
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)
		
		// 최소 디스크 공간 설정 optional
		minFreeDiskSpace := 2 * ethconfig.Defaults.TrieDirtyCache // Default 2 * 256Mb
		if ctx.IsSet(MinFreeDiskSpaceFlag.Name) {
			minFreeDiskSpace = ctx.Int(MinFreeDiskSpaceFlag.Name)
		} else if ctx.IsSet(CacheFlag.Name) || ctx.IsSet(CacheGCFlag.Name) {
			minFreeDiskSpace = 2 * ctx.Int(CacheFlag.Name) * ctx.Int(CacheGCFlag.Name) / 100
		}

		// 디스크 여유공간이 부족해지면 로그 경고 출력용 고루틴 실행
		if minFreeDiskSpace > 0 {
			// 아래의 고루틴이 내부적으로 반복적으로 디스크 상태를 감시하는 루프를 돌린다.
			go monitorFreeDiskSpace(sigc, stack.InstanceDir(), uint64(minFreeDiskSpace)*1024*1024)
		}

		// 노드 종료할떄 실행되는 루틴
		shutdown := func() {
			log.Info("Got interrupt, shutting down...")
			go stack.Close()
			for i := 10; i > 0; i-- {
				<-sigc
				if i > 1 {
					log.Warn("Already shutting down, interrupt more to panic.", "times", i-1)
				}
			}
			debug.Exit() // ensure trace and CPU profile data is flushed.
			debug.LoudPanic("boom")
		}

		if isConsole {
			// In JS console mode, SIGINT is ignored because it's handled by the console.
			// However, SIGTERM still shuts down the node.
			for {
				sig := <-sigc
				if sig == syscall.SIGTERM {
					shutdown()
					return
				}
			}
		} else {
			<-sigc
			shutdown()
		}
	}()
}



// 디스크 경로 path의 사용공간이 freeDiskSpaceCritical 보다 작아지는지 주기적으로 검사한다.
// 이 함수는 startNode 함수안에서 고루틴으로 백그라운드에서 계속 실행된다.
func monitorFreeDiskSpace(sigc chan os.Signal, path string, freeDiskSpaceCritical uint64) {
	if path == "" {
		return
	}
	for {
		freeSpace, err := getFreeDiskSpace(path)
		if err != nil {
			log.Warn("Failed to get free disk space", "path", path, "err", err)
			break
		}

		// 딧크 공간이 부족하다면 로그 출력 + startnode의 고루틴에 signal을 보내고 shutdown호출 -> geth 종료
		if freeSpace < freeDiskSpaceCritical {
			log.Error("Low disk space. Gracefully shutting down Geth to prevent database corruption.", "available", common.StorageSize(freeSpace), "path", path)
			sigc <- syscall.SIGTERM
			break
		} else if freeSpace < 2*freeDiskSpaceCritical {
			log.Warn("Disk space is running low. Geth will shutdown if disk space runs below critical level.", "available", common.StorageSize(freeSpace), "critical_level", common.StorageSize(freeDiskSpaceCritical), "path", path)
		}
		time.Sleep(30 * time.Second)
	}
}



// geth에서 블록체인 데이터를 RLP 포맷으로 저장된 파일에서 읽어와 체인에 삽입하는 기능을 수행하는 함수
// 파일에서 읽은 블록들을 직접 내 로컬 체인에 삽입하는 기능으로 (offline) p2p로 받는게 아닌 오프라인 파일을 통해 체인에 삽이하는 경우
func ImportChain(chain *core.BlockChain, fn string) error {
	// Watch for Ctrl-C while the import is running.
	// If a signal is received, the import will stop at the next batch.
	interrupt := make(chan os.Signal, 1)		//  OS에서 SIGNT or SIGTERM이 들어오면 받는 채널
	stop := make(chan struct{})					// 내부 고루틴간 통신용
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	defer close(interrupt)

	// 고루틴으로 백그라운드에서 interrupt를 감시함  	interrupt에 시그널이 들어오면 stop 채널을 닫는다.
	go func() {
		if _, ok := <-interrupt; ok {
			log.Info("Interrupted during import, stopping at next batch")
		}
		close(stop)
	}()
	// stop 채널이 닫혔는지 확인하는 메서드
	checkInterrupt := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}

	log.Info("Importing blockchain", "file", fn)

	// Open the file handle and potentially unwrap the gzip stream
	// fn경로에서 파일을 열고  
	fh, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer fh.Close()
	
	// gzip이면 압축 해제 처리
	var reader io.Reader = fh
	if strings.HasSuffix(fn, ".gz") {
		if reader, err = gzip.NewReader(reader); err != nil {
			return err
		}
	}
	
	// rlp.NewStream으로 RLP 디코딩 스트림 생성		*직렬화된 블록 데이터를 -> 구조체로 디코딩하는 스트림
	stream := rlp.NewStream(reader, 0)

	// Run actual the import. 블록을 배치단위로 읽고 체인에 Import
	blocks := make(types.Blocks, importBatchSize)
	n := 0
	for batch := 0; ; batch++ {
		// Load a batch of RLP blocks.
		if checkInterrupt() {				// interrupt check
			return ErrImportInterrupted
		}

		// RLP block decoding
		i := 0
		for ; i < importBatchSize; i++ {
			var b types.Block
			if err := stream.Decode(&b); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("at block %d: %v", n, err)
			}
			// don't import first block
			if b.NumberU64() == 0 {
				i--
				continue
			}
			blocks[i] = &b
			n++
		}
		if i == 0 {
			break
		}
		// Import the batch.
		if checkInterrupt() {
			return errors.New("interrupted")
		}

		// 실제로 필요한 블록만 필터링
		missing := missingBlocks(chain, blocks[:i])
		if len(missing) == 0 {
			log.Info("Skipping batch as all blocks present", "batch", batch, "first", blocks[0].Hash(), "last", blocks[i-1].Hash())
			continue
		}

		// 체인에 블록 삽입		 이떄 유효성검사도 하면서 넣는다.
		if failindex, err := chain.InsertChain(missing); err != nil {
			var failnumber uint64
			if failindex > 0 && failindex < len(missing) {
				failnumber = missing[failindex].NumberU64()
			} else {
				failnumber = missing[0].NumberU64()
			}
			return fmt.Errorf("invalid block %d: %v", failnumber, err)
		}
	}
	return nil
}



// 텍스트 파일을 읽어서 줄 단위 문자열 배열로 반환하는 함수 util
func readList(filename string) ([]string, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}



// ImportHistory imports Era1 files containing historical block information,
// starting from genesis. The assumption is held that the provided chain
// segment in Era1 file should all be canonical and verified.

// Era1 형식의 히스토리 블록 데이터파일을 가져와 체인에 삽입하는 함수
func ImportHistory(chain *core.BlockChain, dir string, network string) error {
	if chain.CurrentSnapBlock().Number.BitLen() != 0 {
		return errors.New("history import only supported when starting from genesis")
	}
	entries, err := era.ReadDir(dir, network)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", dir, err)
	}
	checksums, err := readList(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return fmt.Errorf("unable to read checksums.txt: %w", err)
	}
	if len(checksums) != len(entries) {
		return fmt.Errorf("expected equal number of checksums and entries, have: %d checksums, %d entries", len(checksums), len(entries))
	}
	var (
		start    = time.Now()
		reported = time.Now()
		imported = 0
		h        = sha256.New()
		buf      = bytes.NewBuffer(nil)
	)
	for i, filename := range entries {
		err := func() error {
			f, err := os.Open(filepath.Join(dir, filename))
			if err != nil {
				return fmt.Errorf("unable to open era: %w", err)
			}
			defer f.Close()

			// Validate checksum.
			if _, err := io.Copy(h, f); err != nil {
				return fmt.Errorf("unable to recalculate checksum: %w", err)
			}
			if have, want := common.BytesToHash(h.Sum(buf.Bytes()[:])).Hex(), checksums[i]; have != want {
				return fmt.Errorf("checksum mismatch: have %s, want %s", have, want)
			}
			h.Reset()
			buf.Reset()

			// Import all block data from Era1.
			e, err := era.From(f)
			if err != nil {
				return fmt.Errorf("error opening era: %w", err)
			}
			it, err := era.NewIterator(e)
			if err != nil {
				return fmt.Errorf("error making era reader: %w", err)
			}
			for it.Next() {
				block, err := it.Block()
				if err != nil {
					return fmt.Errorf("error reading block %d: %w", it.Number(), err)
				}
				if block.Number().BitLen() == 0 {
					continue // skip genesis
				}
				receipts, err := it.Receipts()
				if err != nil {
					return fmt.Errorf("error reading receipts %d: %w", it.Number(), err)
				}
				encReceipts := types.EncodeBlockReceiptLists([]types.Receipts{receipts})
				if _, err := chain.InsertReceiptChain([]*types.Block{block}, encReceipts, 2^64-1); err != nil {
					return fmt.Errorf("error inserting body %d: %w", it.Number(), err)
				}
				imported += 1

				// Give the user some feedback that something is happening.
				if time.Since(reported) >= 8*time.Second {
					log.Info("Importing Era files", "head", it.Number(), "imported", imported, "elapsed", common.PrettyDuration(time.Since(start)))
					imported = 0
					reported = time.Now()
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}



// 주어진 블록 리스트 중에서 아직 로컬체인에 없는 블록들만 골라내는 함수
func missingBlocks(chain *core.BlockChain, blocks []*types.Block) []*types.Block {
	head := chain.CurrentBlock()	// 현재 로컬체인의 head 블록 *현재 체인의 상태를 가져옴
	for i, block := range blocks {
		// If we're behind the chain head, only check block, state is available at head
		// head 즉, 현재 로컬의 최신블록 보다  이전블록의 경우는 존재하기만하면 된다. 없다면 리턴
		if head.Number.Uint64() > block.NumberU64() {
			if !chain.HasBlock(block.Hash(), block.NumberU64()) {
				return blocks[i:]
			}
			continue
		}
		// If we're above the chain head, state availability is a must
		// 현재 head 이상이라면 블록 + 상태 둘 다 존재해야한다. 그 시점부터 전부 리턴함
		if !chain.HasBlockAndState(block.Hash(), block.NumberU64()) {
			return blocks[i:]
		}
	}
	return nil
}



// ExportChain exports a blockchain into the specified file, truncating any data
// already present in the file.
// Geth에서 블록체인 데이터를 로컬에 파일로 저장할 떄 사용하는 함수
func ExportChain(blockchain *core.BlockChain, fn string) error {
	log.Info("Exporting blockchain", "file", fn)		// 어디에 export 저장하는지 로그를 남김

	// Open the file handle and potentially wrap with a gzip stream
	// 파일을 연다.
	fh, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	defer fh.Close()

	// 파일이 gzip인지 아닌지에 따라서 writer를 골라서 압축처리
	var writer io.Writer = fh
	if strings.HasSuffix(fn, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}
	// Iterate over the blocks and export them
	// export 실행 (RLP 직렬화여 순차적으로 기록)
	if err := blockchain.Export(writer); err != nil {
		return err
	}
	log.Info("Exported blockchain", "file", fn)		// 성공 로그

	return nil
}



// ExportAppendChain exports a blockchain into the specified file, appending to
// the file if data already exists in it.
// Geth에서 블록체인 데이터를 로컬에 파일로 부분적으로 저장할때 사용하는 함수
func ExportAppendChain(blockchain *core.BlockChain, fn string, first uint64, last uint64) error {
	log.Info("Exporting blockchain", "file", fn)

	// Open the file handle and potentially wrap with a gzip stream
	fh, err := os.OpenFile(fn, os.O_CREATE|os.O_APPEND|os.O_WRONLY, os.ModePerm)
	if err != nil {
		return err
	}
	defer fh.Close()

	var writer io.Writer = fh
	if strings.HasSuffix(fn, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}
	// Iterate over the blocks and export them
	// ***  부분적으로 export
	if err := blockchain.ExportN(writer, first, last); err != nil {
		return err
	}
	log.Info("Exported blockchain to", "file", fn)
	return nil
}



// ExportHistory exports blockchain history into the specified directory,
// following the Era format.
// 블록체인 히스토리를 era1포멧으로 구간별로 export 저장하는 함수
func ExportHistory(bc *core.BlockChain, dir string, first, last, step uint64) error {
	log.Info("Exporting blockchain history", "dir", dir)
	if head := bc.CurrentBlock().Number.Uint64(); head < last {
		log.Warn("Last block beyond head, setting last = head", "head", head, "last", last)
		last = head
	}
	network := "unknown"
	if name, ok := params.NetworkNames[bc.Config().ChainID.String()]; ok {
		network = name
	}
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating output directory: %w", err)
	}
	var (
		start     = time.Now()
		reported  = time.Now()
		h         = sha256.New()
		buf       = bytes.NewBuffer(nil)
		checksums []string
	)
	td := new(big.Int)
	for i := uint64(0); i < first; i++ {
		td.Add(td, bc.GetHeaderByNumber(i).Difficulty)
	}
	for i := first; i <= last; i += step {
		err := func() error {
			filename := filepath.Join(dir, era.Filename(network, int(i/step), common.Hash{}))
			f, err := os.Create(filename)
			if err != nil {
				return fmt.Errorf("could not create era file: %w", err)
			}
			defer f.Close()

			w := era.NewBuilder(f)
			for j := uint64(0); j < step && j <= last-i; j++ {
				var (
					n     = i + j
					block = bc.GetBlockByNumber(n)
				)
				if block == nil {
					return fmt.Errorf("export failed on #%d: not found", n)
				}
				receipts := bc.GetReceiptsByHash(block.Hash())
				if receipts == nil {
					return fmt.Errorf("export failed on #%d: receipts not found", n)
				}
				td.Add(td, block.Difficulty())
				if err := w.Add(block, receipts, new(big.Int).Set(td)); err != nil {
					return err
				}
			}
			root, err := w.Finalize()
			if err != nil {
				return fmt.Errorf("export failed to finalize %d: %w", step/i, err)
			}
			// Set correct filename with root.
			os.Rename(filename, filepath.Join(dir, era.Filename(network, int(i/step), root)))

			// Compute checksum of entire Era1.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.Copy(h, f); err != nil {
				return fmt.Errorf("unable to calculate checksum: %w", err)
			}
			checksums = append(checksums, common.BytesToHash(h.Sum(buf.Bytes()[:])).Hex())
			h.Reset()
			buf.Reset()
			return nil
		}()
		if err != nil {
			return err
		}
		if time.Since(reported) >= 8*time.Second {
			log.Info("Exporting blocks", "exported", i, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}

	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(checksums, "\n")), os.ModePerm)

	log.Info("Exported blockchain to", "dir", dir)

	return nil
}



// ImportPreimages imports a batch of exported hash preimages into the database.
// It's a part of the deprecated functionality, should be removed in the future.
// Keccak 해시의 preimage(원본 데이터)를 디코딩해서 DB에 저장하는 함수		** keccak256 has -> preimage(원시 데이터) 형태의 매핑을 디스크에 저장
// 즉, 원본데이터를 해시하고, 그 값과 원시데이터끼리 매핑을 통해서   해시만 저장하는 경우도 매핑을 통해서 원시 데이터를 찾을 수 있게 함.
func ImportPreimages(db ethdb.Database, fn string) error {
	log.Info("Importing preimages", "file", fn)

	// Open the file handle and potentially unwrap the gzip stream
	// 파일 열기  + gzip 처리
	fh, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer fh.Close()

	var reader io.Reader = bufio.NewReader(fh)
	if strings.HasSuffix(fn, ".gz") {
		if reader, err = gzip.NewReader(reader); err != nil {
			return err
		}
	}

	// RLP 스트림 디코딩 준비
	stream := rlp.NewStream(reader, 0)

	// Import the preimages in batches to prevent disk thrashing
	preimages := make(map[common.Hash][]byte)

	for {
		// Read the next entry and ensure it's not junk
		var blob []byte

		if err := stream.Decode(&blob); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		// Accumulate the preimages and flush when enough ws gathered
		preimages[crypto.Keccak256Hash(blob)] = common.CopyBytes(blob)
		if len(preimages) > 1024 {
			rawdb.WritePreimages(db, preimages)
			preimages = make(map[common.Hash][]byte)
		}
	}
	// Flush the last batch preimage data
	if len(preimages) > 0 {
		rawdb.WritePreimages(db, preimages)
	}
	return nil
}



// ExportPreimages exports all known hash preimages into the specified file,
// truncating any data already present in the file.
// It's a part of the deprecated functionality, should be removed in the future.
// preimage DB -> 파일로 내보내는 함수 export key-vale 을 일반 file로 내보냄
func ExportPreimages(db ethdb.Database, fn string) error {
	log.Info("Exporting preimages", "file", fn)

	// Open the file handle and potentially wrap with a gzip stream
	fh, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	defer fh.Close()

	var writer io.Writer = fh
	if strings.HasSuffix(fn, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}
	// Iterate over the preimages and export them
	it := db.NewIterator([]byte("secure-key-"), nil)
	defer it.Release()

	for it.Next() {
		if err := rlp.Encode(writer, it.Value()); err != nil {
			return err
		}
	}
	log.Info("Exported preimages", "file", fn)
	return nil
}




// ExportSnapshotPreimages exports the preimages corresponding to the enumeration of
// the snapshot for a given root.
// TODO: snapshot.Tree 스냅샷 트리에서 preimage 데이터들을 추출하여서 RLP로 인코딩하고 파일로 저장하는 함수
// 	     스냅샷 트리에는 key들이 keccak256 해시값만 저장되어있기 때문에, 그에 해당하는 원래 preimages을 DB에서 찾아 export하는 함수
// *** account의 hash를 통해서 preimage를 읽어오는 것을 gorutin으로 하여서 조회 + 저장을 병렬적으로 처리한다.
func ExportSnapshotPreimages(chaindb ethdb.Database, snaptree *snapshot.Tree, fn string, root common.Hash) error {
	log.Info("Exporting preimages", "file", fn)

	// 파일 열기
	fh, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	defer fh.Close()

	// Enable gzip compressing if file name has gz suffix.
	// 압축준비
	var writer io.Writer = fh
	if strings.HasSuffix(fn, ".gz") {
		gz := gzip.NewWriter(writer)
		defer gz.Close()
		writer = gz
	}
	buf := bufio.NewWriter(writer)
	defer buf.Flush()
	writer = buf


	type hashAndPreimageSize struct {
		Hash common.Hash
		Size int
	}
	hashCh := make(chan hashAndPreimageSize)

	var (
		start     = time.Now()
		logged    = time.Now()
		preimages int
	)

	// 고루틴으로 각 계정의 hash를 통해서 preimage 수집
	go func() {
		defer close(hashCh)
		accIt, err := snaptree.AccountIterator(root, common.Hash{})
		if err != nil {
			log.Error("Failed to create account iterator", "error", err)
			return
		}
		defer accIt.Release()

		for accIt.Next() {
			acc, err := types.FullAccount(accIt.Account())
			if err != nil {
				log.Error("Failed to get full account", "error", err)
				return
			}
			preimages += 1
			hashCh <- hashAndPreimageSize{Hash: accIt.Hash(), Size: common.AddressLength}

			if acc.Root != (common.Hash{}) && acc.Root != types.EmptyRootHash {
				stIt, err := snaptree.StorageIterator(root, accIt.Hash(), common.Hash{})
				if err != nil {
					log.Error("Failed to create storage iterator", "error", err)
					return
				}
				for stIt.Next() {
					preimages += 1
					hashCh <- hashAndPreimageSize{Hash: stIt.Hash(), Size: common.HashLength}

					if time.Since(logged) > time.Second*8 {
						logged = time.Now()
						log.Info("Exporting preimages", "count", preimages, "elapsed", common.PrettyDuration(time.Since(start)))
					}
				}
				stIt.Release()
			}
			if time.Since(logged) > time.Second*8 {
				logged = time.Now()
				log.Info("Exporting preimages", "count", preimages, "elapsed", common.PrettyDuration(time.Since(start)))
			}
		}
	}()

	// preiamge 저장
	for item := range hashCh {
		preimage := rawdb.ReadPreimage(chaindb, item.Hash)
		if len(preimage) == 0 {
			return fmt.Errorf("missing preimage for %v", item.Hash)
		}
		if len(preimage) != item.Size {
			return fmt.Errorf("invalid preimage size, have %d", len(preimage))
		}
		rlpenc, err := rlp.EncodeToBytes(preimage)
		if err != nil {
			return fmt.Errorf("error encoding preimage: %w", err)
		}
		if _, err := writer.Write(rlpenc); err != nil {
			return fmt.Errorf("failed to write preimage: %w", err)
		}
	}
	log.Info("Exported preimages", "count", preimages, "elapsed", common.PrettyDuration(time.Since(start)), "file", fn)
	return nil
}






// exportHeader is used in the export/import flow. When we do an export,
// the first element we output is the exportHeader.
// Whenever a backwards-incompatible change is made, the Version header
// should be bumped.
// If the importer sees a higher version, it should reject the import.


/*
	exportHeader : export된 파일의 메타정보 블록으로 파일 맨 앞에 RLP 인코딩이 되어 있다.
*/
type exportHeader struct {
	Magic    string // Always set to 'gethdbdump' for disambiguation   포맷 식별자
	Version  uint64 // 포맷버전
	Kind     string // 데이터 종류	preimage, snapshot
	UnixTime uint64 // export 타임
}

const exportMagic = "gethdbdump"
// export된 데이터에서 각 키-값 쌍을 어떤 방식으로 처리할지 명시하는 것    0 : DB에 추가		1 : DB에서 삭제
const (
	OpBatchAdd = 0
	OpBatchDel = 1
)




// ImportLDBData imports a batch of snapshot data into the database
// TODO: export된 preimage나 snapshot 데이터를 디스크에서 읽어서 LevelDB에 다시 삽입하는 import 함수
// 	     ExportSnapShotPreimages()의 반대작업이다.
func ImportLDBData(db ethdb.Database, f string, startIndex int64, interrupt chan struct{}) error {
	log.Info("Importing leveldb data", "file", f)

	// Open the file handle and potentially unwrap the gzip stream
	fh, err := os.Open(f)
	if err != nil {
		return err
	}
	defer fh.Close()

	var reader io.Reader = bufio.NewReader(fh)
	if strings.HasSuffix(f, ".gz") {
		if reader, err = gzip.NewReader(reader); err != nil {
			return err
		}
	}
	stream := rlp.NewStream(reader, 0)

	// Read the header
	var header exportHeader
	if err := stream.Decode(&header); err != nil {
		return fmt.Errorf("could not decode header: %v", err)
	}
	if header.Magic != exportMagic {
		return errors.New("incompatible data, wrong magic")
	}
	if header.Version != 0 {
		return fmt.Errorf("incompatible version %d, (support only 0)", header.Version)
	}
	log.Info("Importing data", "file", f, "type", header.Kind, "data age",
		common.PrettyDuration(time.Since(time.Unix(int64(header.UnixTime), 0))))

	// Import the snapshot in batches to prevent disk thrashing
	var (
		count  int64
		start  = time.Now()
		logged = time.Now()
		batch  = db.NewBatch()
	)
	for {
		// Read the next entry
		var (
			op       byte
			key, val []byte
		)
		if err := stream.Decode(&op); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if err := stream.Decode(&key); err != nil {
			return err
		}
		if err := stream.Decode(&val); err != nil {
			return err
		}
		if count < startIndex {
			count++
			continue
		}
		switch op {
		case OpBatchDel:
			batch.Delete(key)
		case OpBatchAdd:
			batch.Put(key, val)
		default:
			return fmt.Errorf("unknown op %d", op)
		}
		if batch.ValueSize() > ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
		}
		// Check interruption emitted by ctrl+c
		if count%1000 == 0 {
			select {
			case <-interrupt:
				if err := batch.Write(); err != nil {
					return err
				}
				log.Info("External data import interrupted", "file", f, "count", count, "elapsed", common.PrettyDuration(time.Since(start)))
				return nil
			default:
			}
		}
		if count%1000 == 0 && time.Since(logged) > 8*time.Second {
			log.Info("Importing external data", "file", f, "count", count, "elapsed", common.PrettyDuration(time.Since(start)))
			logged = time.Now()
		}
		count += 1
	}
	// Flush the last batch snapshot data
	if batch.ValueSize() > 0 {
		if err := batch.Write(); err != nil {
			return err
		}
	}
	log.Info("Imported chain data", "file", f, "count", count,
		"elapsed", common.PrettyDuration(time.Since(start)))
	return nil
}




// ChainDataIterator is an interface wraps all necessary functions to iterate
// the exporting chain data.
type ChainDataIterator interface {
	// Next returns the key-value pair for next exporting entry in the iterator.
	// When the end is reached, it will return (0, nil, nil, false).
	Next() (byte, []byte, []byte, bool)

	// Release releases associated resources. Release should always succeed and can
	// be called multiple times without causing error.
	Release()
}

// ExportChaindata exports the given data type (truncating any data already present)
// in the file. If the suffix is 'gz', gzip compression is used.
// 이더리움 데이터 (preimage, shapshot)등을 .ldb 파일로 보내는 범용적인 export 함수이다.
// 어떤 종류의 체인데이터도 export할 수 있게 만든 추상화 된 버전
func ExportChaindata(fn string, kind string, iter ChainDataIterator, interrupt chan struct{}) error {
	log.Info("Exporting chain data", "file", fn, "kind", kind)
	defer iter.Release()

	// Open the file handle and potentially wrap with a gzip stream
	fh, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	defer fh.Close()

	var writer io.Writer = fh
	if strings.HasSuffix(fn, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}
	// Write the header
	if err := rlp.Encode(writer, &exportHeader{
		Magic:    exportMagic,
		Version:  0,
		Kind:     kind,
		UnixTime: uint64(time.Now().Unix()),
	}); err != nil {
		return err
	}
	// Extract data from source iterator and dump them out to file
	var (
		count  int64
		start  = time.Now()
		logged = time.Now()
	)
	for {
		op, key, val, ok := iter.Next()
		if !ok {
			break
		}
		if err := rlp.Encode(writer, op); err != nil {
			return err
		}
		if err := rlp.Encode(writer, key); err != nil {
			return err
		}
		if err := rlp.Encode(writer, val); err != nil {
			return err
		}
		if count%1000 == 0 {
			// Check interruption emitted by ctrl+c
			select {
			case <-interrupt:
				log.Info("Chain data exporting interrupted", "file", fn,
					"kind", kind, "count", count, "elapsed", common.PrettyDuration(time.Since(start)))
				return nil
			default:
			}
			if time.Since(logged) > 8*time.Second {
				log.Info("Exporting chain data", "file", fn, "kind", kind,
					"count", count, "elapsed", common.PrettyDuration(time.Since(start)))
				logged = time.Now()
			}
		}
		count++
	}
	log.Info("Exported chain data", "file", fn, "kind", kind, "count", count,
		"elapsed", common.PrettyDuration(time.Since(start)))
	return nil
}
