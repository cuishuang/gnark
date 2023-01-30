// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package cs

import (
	"fmt"
	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr"
	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/gkr"
	"github.com/consensys/gnark-crypto/ecc/bls12-377/fr/polynomial"
	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark-crypto/utils"
	"github.com/consensys/gnark/backend/hint"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/std/utils/algo_utils"
	"hash"
	"math/big"
	"time"
)

type gkrSolvingData struct {
	assignments gkr.WireAssignment
	circuit     gkr.Circuit
	memoryPool  polynomial.Pool
	workers     utils.WorkerPool
}

func convertCircuit(noPtr constraint.GkrCircuit) gkr.Circuit {
	resCircuit := make(gkr.Circuit, len(noPtr))
	for i := range noPtr {
		resCircuit[i].Gate = GkrGateRegistry[noPtr[i].Gate]
		resCircuit[i].Inputs = algo_utils.Map(noPtr[i].Inputs, algo_utils.SlicePtrAt(resCircuit))
	}
	return resCircuit
}

func (d *gkrSolvingData) init(info constraint.GkrInfo) gkrAssignment {
	d.circuit = convertCircuit(info.Circuit)
	d.memoryPool = polynomial.NewPool(d.circuit.MemoryRequirements(info.NbInstances)...)
	d.workers = utils.NewWorkerPool()

	assignmentsSequential := make(gkrAssignment, len(d.circuit))
	d.assignments = make(gkr.WireAssignment, len(d.circuit))
	for i := range assignmentsSequential {
		assignmentsSequential[i] = d.memoryPool.Make(info.NbInstances)
		d.assignments[&d.circuit[i]] = assignmentsSequential[i]
	}

	return assignmentsSequential
}

func (d *gkrSolvingData) dumpAssignments() {
	for _, p := range d.assignments {
		d.memoryPool.Dump(p)
	}
}

// this module assumes that wire and instance indexes respect dependencies

type gkrAssignment [][]fr.Element //gkrAssignment is indexed wire first, instance second

func (a gkrAssignment) setOuts(circuit constraint.GkrCircuit, outs []*big.Int) {
	outsI := 0
	for i := range circuit {
		if circuit[i].IsOutput() {
			for j := range a[i] {
				a[i][j].BigInt(outs[outsI])
				outsI++
			}
		}
	}
	// Check if outsI == len(outs)?
}

const log = true

func gkrSolveHint(info constraint.GkrInfo, solvingData *gkrSolvingData) hint.Function {
	return func(_ *big.Int, ins, outs []*big.Int) error {

		startTime := time.Now().UnixMicro()

		// assumes assignmentVector is arranged wire first, instance second in order of solution
		circuit := info.Circuit
		nbInstances := info.NbInstances
		offsets := info.AssignmentOffsets()
		assignment := solvingData.init(info)
		chunks := circuit.Chunks(nbInstances)

		solveTask := func(chunkOffset int) utils.Task {
			return func(startInChunk, endInChunk int) {
				start := startInChunk + chunkOffset
				end := endInChunk + chunkOffset
				inputs := solvingData.memoryPool.Make(info.MaxNIns)
				dependencyHeads := make([]int, len(circuit))
				for wI, w := range circuit {
					dependencyHeads[wI] = algo_utils.BinarySearchFunc(func(i int) int {
						return w.Dependencies[i].InputInstance
					}, len(w.Dependencies), start)
				}

				for instanceI := start; instanceI < end; instanceI++ {
					for wireI, wire := range circuit {
						if wire.IsInput() {
							if dependencyHeads[wireI] < len(wire.Dependencies) && instanceI == wire.Dependencies[dependencyHeads[wireI]].InputInstance {
								dep := wire.Dependencies[dependencyHeads[wireI]]
								assignment[wireI][instanceI].Set(&assignment[dep.OutputWire][dep.OutputInstance])
								dependencyHeads[wireI]++
							} else {
								assignment[wireI][instanceI].SetBigInt(ins[offsets[wireI]+instanceI-dependencyHeads[wireI]])
							}
						} else {
							// assemble the inputs
							inputIndexes := info.Circuit[wireI].Inputs
							for i, inputI := range inputIndexes {
								inputs[i].Set(&assignment[inputI][instanceI])
							}
							gate := solvingData.circuit[wireI].Gate
							assignment[wireI][instanceI] = gate.Evaluate(inputs[:len(inputIndexes)]...)
						}
					}
				}
				solvingData.memoryPool.Dump(inputs)
			}
		}

		start := 0
		for _, end := range chunks {
			solvingData.workers.Dispatch(end-start, 1024, solveTask(start)).Wait()
			start = end
		}

		assignment.setOuts(info.Circuit, outs)

		if log {
			endTime := time.Now().UnixMicro()
			fmt.Println("gkr proved in", endTime-startTime, "μs")
		}

		return nil
	}
}

func frToBigInts(dst []*big.Int, src []fr.Element) {
	for i := range src {
		src[i].BigInt(dst[i])
	}
}

func gkrProveHint(hashName string, data *gkrSolvingData) hint.Function {

	return func(_ *big.Int, ins, outs []*big.Int) error {

		startTime := time.Now().UnixMicro()

		insBytes := algo_utils.Map(ins[1:], func(i *big.Int) []byte { // the first input is dummy, just to ensure the solver's work is done before the prover is called
			b := i.Bytes()
			return b[:]
		})

		hsh := HashBuilderRegistry[hashName]()

		proof, err := gkr.Prove(data.circuit, data.assignments, fiatshamir.WithHash(hsh, insBytes...), gkr.WithPool(&data.memoryPool), gkr.WithWorkers(&data.workers))
		if err != nil {
			return err
		}

		// serialize proof: TODO: In gnark-crypto?
		offset := 0
		for i := range proof {
			for _, poly := range proof[i].PartialSumPolys {
				frToBigInts(outs[offset:], poly)
				offset += len(poly)
			}
			if proof[i].FinalEvalProof != nil {
				finalEvalProof := proof[i].FinalEvalProof.([]fr.Element)
				frToBigInts(outs[offset:], finalEvalProof)
				offset += len(finalEvalProof)
			}
		}

		data.dumpAssignments()

		endTime := time.Now().UnixMicro()
		fmt.Println("gkr solved in", endTime-startTime, "μs")

		return nil

	}
}

func defineGkrHints(info constraint.GkrInfo, hintFunctions map[hint.ID]hint.Function) map[hint.ID]hint.Function {
	res := make(map[hint.ID]hint.Function, len(hintFunctions)+2)
	for k, v := range hintFunctions {
		res[k] = v
	}

	var gkrData gkrSolvingData
	res[info.SolveHintID] = gkrSolveHint(info, &gkrData)
	res[info.ProveHintID] = gkrProveHint(info.HashName, &gkrData)
	return res
}

var GkrGateRegistry = map[string]gkr.Gate{ // TODO: Migrate to gnark-crypto
	"mul": mulGate(2),
	"add": addGate{},
	"sub": subGate{},
	"neg": negGate{},
}

// TODO: Move to gnark-crypto
var HashBuilderRegistry = make(map[string]func() hash.Hash)

type mulGate int
type addGate struct{}
type subGate struct{}
type negGate struct{}

func (g mulGate) Evaluate(x ...fr.Element) (res fr.Element) {
	if len(x) != int(g) {
		panic("wrong input count")
	}
	switch len(x) {
	case 0:
		res.SetOne()
	case 1:
		res.Set(&x[0])
	default:
		res.Mul(&x[0], &x[1])
		for i := 2; i < len(x); i++ {
			res.Mul(&res, &x[2])
		}
	}
	return
}

func (g mulGate) Degree() int {
	return int(g)
}

func (g addGate) Evaluate(x ...fr.Element) (res fr.Element) {
	switch len(x) {
	case 0:
	// set zero
	case 1:
		res.Set(&x[0])
	case 2:
		res.Add(&x[0], &x[1])
		for i := 2; i < len(x); i++ {
			res.Add(&res, &x[2])
		}
	}
	return
}

func (g addGate) Degree() int {
	return 1
}

func (g subGate) Evaluate(element ...fr.Element) (diff fr.Element) {
	if len(element) > 2 {
		panic("not implemented") //TODO
	}
	diff.Sub(&element[0], &element[1])
	return
}

func (g subGate) Degree() int {
	return 1
}

func (g negGate) Evaluate(element ...fr.Element) (neg fr.Element) {
	if len(element) != 1 {
		panic("univariate gate")
	}
	neg.Neg(&element[0])
	return
}

func (g negGate) Degree() int {
	return 1
}
