// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package pow

import (
	"encoding/binary"
	"math/big"
	"reflect"
	"runtime"
	"sync"
	"unsafe"
)

// seedHash is the seed to use for generating a verification cache and the mining
// dataset.
func seedHash(block uint64, epochLength uint64) []byte {
	seed := make([]byte, 32)
	if block < epochLength {
		return seed
	}
	keccak256Hasher := newKeccak256Hasher()
	for i := 0; i < int(block/epochLength); i++ {
		keccak256Hasher(seed, seed)
	}
	return seed
}

// calcEpoch returns the epoch for a given block number (ECIP-1099)
func calcEpoch(block uint64, epochLength uint64) uint64 {
	epoch := block / epochLength
	return epoch
}

// cacheSize returns the size of the ethash verification cache that belongs to a certain
// block number.
func cacheSize(epoch uint64) uint64 {
	if epoch < maxEpoch {
		return cacheSizes[int(epoch)]
	}
	return calcCacheSize(epoch)
}

// calcCacheSize calculates the cache size for epoch. The cache size grows linearly,
// however, we always take the highest prime below the linearly growing threshold in order
// to reduce the risk of accidental regularities leading to cyclic behavior.
func calcCacheSize(epoch uint64) uint64 {
	size := cacheInitBytes + cacheGrowthBytes*epoch - hashBytes
	for !new(big.Int).SetUint64(size / hashBytes).ProbablyPrime(1) { // Always accurate for n < 2^64
		size -= 2 * hashBytes
	}
	return size
}

// datasetSize returns the size of the ethash mining dataset that belongs to a certain
// block number.
func datasetSize(epoch uint64) uint64 {
	if epoch < maxEpoch {
		return datasetSizes[int(epoch)]
	}
	return calcDatasetSize(epoch)
}

// calcDatasetSize calculates the dataset size for epoch. The dataset size grows linearly,
// however, we always take the highest prime below the linearly growing threshold in order
// to reduce the risk of accidental regularities leading to cyclic behavior.
func calcDatasetSize(epoch uint64) uint64 {
	size := datasetInitBytes + datasetGrowthBytes*epoch - mixBytes
	for !new(big.Int).SetUint64(size / mixBytes).ProbablyPrime(1) { // Always accurate for n < 2^64
		size -= 2 * mixBytes
	}
	return size
}

// generateCache creates a verification cache of a given size for an input seed.
// The cache production process involves first sequentially filling up 32 MB of
// memory, then performing two passes of Sergio Demian Lerner's RandMemoHash
// algorithm from Strict Memory Hard Hashing Functions (2014). The output is a
// set of 524288 64-byte values.
// This method places the result into dest in machine byte order.
func generateCache(dest []uint32, epoch uint64, epochLength uint64, seed []byte) {
	// Convert our destination slice to a byte buffer
	header := *(*reflect.SliceHeader)(unsafe.Pointer(&dest))
	header.Len *= 4
	header.Cap *= 4
	cache := *(*[]byte)(unsafe.Pointer(&header))

	// Calculate the number of theoretical rows (we'll store in one buffer nonetheless)
	size := uint64(len(cache))
	rows := int(size) / hashBytes

	// Create a hasher to reuse between invocations
	keccak512Hasher := newKeccak512Hasher()

	// Sequentially produce the initial dataset
	keccak512Hasher(cache, seed)
	for offset := uint64(hashBytes); offset < size; offset += hashBytes {
		keccak512Hasher(cache[offset:], cache[offset-hashBytes:offset])
	}

	// Use a low-round version of randmemohash
	temp := make([]byte, hashBytes)

	for i := 0; i < cacheRounds; i++ {
		for j := 0; j < rows; j++ {
			var (
				srcOff = ((j - 1 + rows) % rows) * hashBytes
				dstOff = j * hashBytes
				xorOff = (binary.LittleEndian.Uint32(cache[dstOff:]) % uint32(rows)) * hashBytes
			)
			xorBytes(temp, cache[srcOff:srcOff+hashBytes], cache[xorOff:xorOff+hashBytes])
			keccak512Hasher(cache[dstOff:], temp)
		}
	}
	// Swap the byte order on big endian systems and return
	if !isLittleEndian() {
		swap(cache)
	}
}

func generateL1Cache(dest []uint32, cache []uint32) {
	swapped := !isLittleEndian()

	keccak512Hasher := newKeccak512Hasher()

	header := *(*reflect.SliceHeader)(unsafe.Pointer(&dest))
	header.Len *= 4
	header.Cap *= 4
	l1 := *(*[]byte)(unsafe.Pointer(&header))

	size := uint64(len(l1))
	rows := int(size) / hashBytes

	for i := 0; i < rows; i++ {
		item := generateDatasetItem(cache, uint32(i), keccak512Hasher, 512)
		if swapped {
			swap(item)
		}

		copy(l1[i*hashBytes:], item)
	}
}

// generateDatasetItem combines data from 256 pseudorandomly selected cache nodes,
// and hashes that to compute a single dataset node.
func generateDatasetItem(cache []uint32, index uint32, keccak512Hasher hasher, datasetParents uint32) []byte {
	// Calculate the number of theoretical rows (we use one buffer nonetheless)
	rows := uint32(len(cache) / hashWords)

	// Initialize the mix
	mix := make([]byte, hashBytes)

	binary.LittleEndian.PutUint32(mix, cache[(index%rows)*hashWords]^index)
	for i := 1; i < hashWords; i++ {
		binary.LittleEndian.PutUint32(mix[i*4:], cache[(index%rows)*hashWords+uint32(i)])
	}
	keccak512Hasher(mix, mix)

	// Convert the mix to uint32s to avoid constant bit shifting
	intMix := make([]uint32, hashWords)
	for i := 0; i < len(intMix); i++ {
		intMix[i] = binary.LittleEndian.Uint32(mix[i*4:])
	}
	// fnv it with a lot of random cache nodes based on index
	for i := uint32(0); i < datasetParents; i++ {
		parent := fnv1(index^i, intMix[i%16]) % rows
		fnvHash(intMix, cache[parent*hashWords:])
	}
	// Flatten the uint32 mix into a binary one and return
	for i, val := range intMix {
		binary.LittleEndian.PutUint32(mix[i*4:], val)
	}
	keccak512Hasher(mix, mix)
	return mix
}

func generateDatasetItem512(cache []uint32, index uint32, keccak512Hasher hasher, datasetParents uint32) []uint32 {
	data := make([]uint32, hashWords)
	item := generateDatasetItem(cache, index, keccak512Hasher, datasetParents)

	for i := 0; i < hashWords; i++ {
		data[i] = binary.LittleEndian.Uint32(item[i*4:])
	}

	return data
}

func generateDatasetItem1024(cache []uint32, index uint32, keccak512Hasher hasher, datasetParents uint32) []uint32 {
	data := make([]uint32, hashWords*2)
	for n := 0; n < 2; n++ {
		item := generateDatasetItem(cache, index*2+uint32(n), keccak512Hasher, datasetParents)

		for i := 0; i < hashWords; i++ {
			data[n*hashWords+i] = binary.LittleEndian.Uint32(item[i*4:])
		}
	}

	return data
}

func generateDatasetItem2048(cache []uint32, index uint32, keccak512Hasher hasher, datasetParents uint32) []uint32 {
	data := make([]uint32, hashWords*4)
	for n := 0; n < 4; n++ {
		item := generateDatasetItem(cache, index*4+uint32(n), keccak512Hasher, datasetParents)

		for i := 0; i < hashWords; i++ {
			data[n*hashWords+i] = binary.LittleEndian.Uint32(item[i*4 : i*4+4])
		}
	}

	return data
}

// generateDataset generates the entire ethash dataset for mining.
// This method places the result into dest in machine byte order.
func generateDataset(dest []uint32, epoch uint64, epochLength uint64, cache []uint32, datasetParents uint32) {
	// Figure out whether the bytes need to be swapped for the machine
	swapped := !isLittleEndian()

	// Convert our destination slice to a byte buffer
	header := *(*reflect.SliceHeader)(unsafe.Pointer(&dest))
	header.Len *= 4
	header.Cap *= 4
	dataset := *(*[]byte)(unsafe.Pointer(&header))

	// Generate the dataset on many goroutines since it takes a while
	threads := runtime.NumCPU()
	size := uint64(len(dataset))

	var pend sync.WaitGroup
	pend.Add(threads)

	for i := 0; i < threads; i++ {
		go func(id int) {
			defer pend.Done()

			// Create a hasher to reuse between invocations
			keccak512Hasher := newKeccak512Hasher()

			// Calculate the data segment this thread should generate
			batch := (size + hashBytes*uint64(threads) - 1) / (hashBytes * uint64(threads))
			first := uint64(id) * batch
			limit := first + batch
			if limit > size/hashBytes {
				limit = size / hashBytes
			}
			// Calculate the dataset segment
			for index := first; index < limit; index++ {
				item := generateDatasetItem(cache, uint32(index), keccak512Hasher, datasetParents)
				if swapped {
					swap(item)
				}
				copy(dataset[index*hashBytes:], item)
			}
		}(i)
	}
	// Wait for all the generators to finish and return
	pend.Wait()
}
