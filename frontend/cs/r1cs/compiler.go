/*
Copyright © 2020 ConsenSys

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package r1cs

import (
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/hint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/compiled"
	"github.com/consensys/gnark/frontend/cs"
	"github.com/consensys/gnark/frontend/schema"
	bls12377r1cs "github.com/consensys/gnark/internal/backend/bls12-377/cs"
	bls12381r1cs "github.com/consensys/gnark/internal/backend/bls12-381/cs"
	bls24315r1cs "github.com/consensys/gnark/internal/backend/bls24-315/cs"
	bn254r1cs "github.com/consensys/gnark/internal/backend/bn254/cs"
	bw6633r1cs "github.com/consensys/gnark/internal/backend/bw6-633/cs"
	bw6761r1cs "github.com/consensys/gnark/internal/backend/bw6-761/cs"
	"github.com/consensys/gnark/internal/utils"
)

// NewCompiler returns a new R1CS compiler
func NewCompiler(curve ecc.ID, config frontend.CompileConfig) (frontend.Builder, error) {
	return newCompiler(curve, config), nil
}

type compiler struct {
	compiled.ConstraintSystem
	Constraints []compiled.R1C

	st     cs.CoeffTable
	config frontend.CompileConfig

	// map for recording boolean constrained variables (to not constrain them twice)
	mtBooleans map[uint64][]compiled.LinearExpression
}

// initialCapacity has quite some impact on frontend performance, especially on large circuits size
// we may want to add build tags to tune that
func newCompiler(curveID ecc.ID, config frontend.CompileConfig) *compiler {
	system := compiler{
		ConstraintSystem: compiled.ConstraintSystem{

			MDebug: make(map[int]int),
			MHints: make(map[int]*compiled.Hint),
		},
		Constraints: make([]compiled.R1C, 0, config.Capacity),
		st:          cs.NewCoeffTable(),
		mtBooleans:  make(map[uint64][]compiled.LinearExpression),
		config:      config,
	}

	system.Public = make([]string, 1)
	system.Secret = make([]string, 0)

	// by default the circuit is given a public wire equal to 1
	system.Public[0] = "one"

	system.CurveID = curveID

	return &system
}

// newInternalVariable creates a new wire, appends it on the list of wires of the circuit, sets
// the wire's id to the number of wires, and returns it
func (system *compiler) newInternalVariable() compiled.Variable {
	idx := system.NbInternalVariables
	system.NbInternalVariables++
	return compiled.Variable{
		LinExp: compiled.LinearExpression{compiled.Pack(idx, compiled.CoeffIdOne, schema.Internal)},
	}
}

// AddPublicVariable creates a new public Variable
func (system *compiler) AddPublicVariable(name string) frontend.Variable {
	if system.Schema != nil {
		panic("do not call AddPublicVariable in circuit.Define()")
	}
	idx := len(system.Public)
	system.Public = append(system.Public, name)
	res := compiled.Variable{
		LinExp: compiled.LinearExpression{compiled.Pack(idx, compiled.CoeffIdOne, schema.Public)},
	}
	return res
}

// AddSecretVariable creates a new secret Variable
func (system *compiler) AddSecretVariable(name string) frontend.Variable {
	if system.Schema != nil {
		panic("do not call AddSecretVariable in circuit.Define()")
	}
	idx := len(system.Secret)
	system.Secret = append(system.Secret, name)
	res := compiled.Variable{
		LinExp: compiled.LinearExpression{compiled.Pack(idx, compiled.CoeffIdOne, schema.Secret)},
	}
	return res
}

func (system *compiler) one() compiled.Variable {
	return compiled.Variable{
		LinExp: compiled.LinearExpression{compiled.Pack(0, compiled.CoeffIdOne, schema.Public)},
	}
}

// reduces redundancy in linear expression
// It factorizes Variable that appears multiple times with != coeff Ids
// To ensure the determinism in the compile process, Variables are stored as public∥secret∥internal∥unset
// for each visibility, the Variables are sorted from lowest ID to highest ID
func (system *compiler) reduce(l compiled.Variable) compiled.Variable {
	// ensure our linear expression is sorted, by visibility and by Variable ID
	if !sort.IsSorted(l.LinExp) { // may not help
		sort.Sort(l.LinExp)
	}

	mod := system.CurveID.Info().Fr.Modulus()
	c := new(big.Int)
	for i := 1; i < len(l.LinExp); i++ {
		pcID, pvID, pVis := l.LinExp[i-1].Unpack()
		ccID, cvID, cVis := l.LinExp[i].Unpack()
		if pVis == cVis && pvID == cvID {
			// we have redundancy
			c.Add(&system.st.Coeffs[pcID], &system.st.Coeffs[ccID])
			c.Mod(c, mod)
			l.LinExp[i-1].SetCoeffID(system.st.CoeffID(c))
			l.LinExp = append(l.LinExp[:i], l.LinExp[i+1:]...)
			i--
		}
	}
	return l
}

// newR1C clones the linear expression associated with the Variables (to avoid offseting the ID multiple time)
// and return a R1C
func newR1C(_l, _r, _o frontend.Variable) compiled.R1C {
	l := _l.(compiled.Variable)
	r := _r.(compiled.Variable)
	o := _o.(compiled.Variable)

	// interestingly, this is key to groth16 performance.
	// l * r == r * l == o
	// but the "l" linear expression is going to end up in the A matrix
	// the "r" linear expression is going to end up in the B matrix
	// the less Variable we have appearing in the B matrix, the more likely groth16.Setup
	// is going to produce infinity points in pk.G1.B and pk.G2.B, which will speed up proving time
	if len(l.LinExp) > len(r.LinExp) {
		l, r = r, l
	}

	return compiled.R1C{L: l.Clone(), R: r.Clone(), O: o.Clone()}
}

func (system *compiler) addConstraint(r1c compiled.R1C, debugID ...int) {
	system.Constraints = append(system.Constraints, r1c)
	if len(debugID) > 0 {
		system.MDebug[len(system.Constraints)-1] = debugID[0]
	}
}

// Term packs a Variable and a coeff in a Term and returns it.
// func (system *R1CSRefactor) setCoeff(v Variable, coeff *big.Int) Term {
func (system *compiler) setCoeff(v compiled.Term, coeff *big.Int) compiled.Term {
	_, vID, vVis := v.Unpack()
	return compiled.Pack(vID, system.st.CoeffID(coeff), vVis)
}

// MarkBoolean sets (but do not **constraint**!) v to be boolean
// This is useful in scenarios where a variable is known to be boolean through a constraint
// that is not api.AssertIsBoolean. If v is a constant, this is a no-op.
func (system *compiler) MarkBoolean(v frontend.Variable) {
	if b, ok := system.ConstantValue(v); ok {
		if !(b.IsUint64() && b.Uint64() <= 1) {
			panic("MarkBoolean called a non-boolean constant")
		}
		return
	}
	// v is a linear expression
	l := v.(compiled.Variable).LinExp
	if !sort.IsSorted(l) {
		sort.Sort(l)
	}

	key := l.HashCode()
	list := system.mtBooleans[key]
	list = append(list, l)
	system.mtBooleans[key] = list
}

// IsBoolean returns true if given variable was marked as boolean in the compiler (see MarkBoolean)
// Use with care; variable may not have been **constrained** to be boolean
// This returns true if the v is a constant and v == 0 || v == 1.
func (system *compiler) IsBoolean(v frontend.Variable) bool {
	if b, ok := system.ConstantValue(v); ok {
		return b.IsUint64() && b.Uint64() <= 1
	}
	// v is a linear expression
	l := v.(compiled.Variable).LinExp
	if !sort.IsSorted(l) {
		sort.Sort(l)
	}

	key := l.HashCode()
	list, ok := system.mtBooleans[key]
	if !ok {
		return false
	}

	for _, v := range list {
		if v.Equal(l) {
			return true
		}
	}
	return false
}

// checkVariables perform post compilation checks on the Variables
//
// 1. checks that all user inputs are referenced in at least one constraint
// 2. checks that all hints are constrained
func (system *compiler) checkVariables() error {

	// TODO @gbotrel add unit test for that.

	cptSecret := len(system.Secret)
	cptPublic := len(system.Public)
	cptHints := len(system.MHints)

	secretConstrained := make([]bool, cptSecret)
	publicConstrained := make([]bool, cptPublic)
	// one wire does not need to be constrained
	publicConstrained[0] = true
	cptPublic--

	mHintsConstrained := make(map[int]bool)

	// for each constraint, we check the linear expressions and mark our inputs / hints as constrained
	processLinearExpression := func(l compiled.Variable) {
		for _, t := range l.LinExp {
			if t.CoeffID() == compiled.CoeffIdZero {
				// ignore zero coefficient, as it does not constraint the Variable
				// though, we may want to flag that IF the Variable doesn't appear else where
				continue
			}
			visibility := t.VariableVisibility()
			vID := t.WireID()

			switch visibility {
			case schema.Public:
				if vID != 0 && !publicConstrained[vID] {
					publicConstrained[vID] = true
					cptPublic--
				}
			case schema.Secret:
				if !secretConstrained[vID] {
					secretConstrained[vID] = true
					cptSecret--
				}
			case schema.Internal:
				if _, ok := system.MHints[vID]; !mHintsConstrained[vID] && ok {
					mHintsConstrained[vID] = true
					cptHints--
				}
			}
		}
	}
	for _, r1c := range system.Constraints {
		processLinearExpression(r1c.L)
		processLinearExpression(r1c.R)
		processLinearExpression(r1c.O)

		if cptHints|cptSecret|cptPublic == 0 {
			return nil // we can stop.
		}

	}

	// something is a miss, we build the error string
	var sbb strings.Builder
	if cptSecret != 0 {
		sbb.WriteString(strconv.Itoa(cptSecret))
		sbb.WriteString(" unconstrained secret input(s):")
		sbb.WriteByte('\n')
		for i := 0; i < len(secretConstrained) && cptSecret != 0; i++ {
			if !secretConstrained[i] {
				sbb.WriteString(system.Secret[i])
				sbb.WriteByte('\n')
				cptSecret--
			}
		}
		sbb.WriteByte('\n')
	}

	if cptPublic != 0 {
		sbb.WriteString(strconv.Itoa(cptPublic))
		sbb.WriteString(" unconstrained public input(s):")
		sbb.WriteByte('\n')
		for i := 0; i < len(publicConstrained) && cptPublic != 0; i++ {
			if !publicConstrained[i] {
				sbb.WriteString(system.Public[i])
				sbb.WriteByte('\n')
				cptPublic--
			}
		}
		sbb.WriteByte('\n')
	}

	if cptHints != 0 {
		sbb.WriteString(strconv.Itoa(cptHints))
		sbb.WriteString(" unconstrained hints")
		sbb.WriteByte('\n')
		// TODO we may add more debug info here → idea, in NewHint, take the debug stack, and store in the hint map some
		// debugInfo to find where a hint was declared (and not constrained)
	}
	return errors.New(sbb.String())
}

var tVariable reflect.Type

func init() {
	tVariable = reflect.ValueOf(struct{ A frontend.Variable }{}).FieldByName("A").Type()
}

// Compile constructs a rank-1 constraint sytem
func (cs *compiler) Compile() (frontend.CompiledConstraintSystem, error) {

	// ensure all inputs and hints are constrained
	if !cs.config.IgnoreUnconstrainedInputs {
		if err := cs.checkVariables(); err != nil {
			return nil, err
		}
	}

	// wires = public wires  | secret wires | internal wires

	// setting up the result
	res := compiled.R1CS{
		ConstraintSystem: cs.ConstraintSystem,
		Constraints:      cs.Constraints,
	}
	res.NbPublicVariables = len(cs.Public)
	res.NbSecretVariables = len(cs.Secret)

	// for Logs, DebugInfo and hints the only thing that will change
	// is that ID of the wires will be offseted to take into account the final wire vector ordering
	// that is: public wires  | secret wires | internal wires

	// offset variable ID depeneding on visibility
	shiftVID := func(oldID int, visibility schema.Visibility) int {
		switch visibility {
		case schema.Internal:
			return oldID + res.NbPublicVariables + res.NbSecretVariables
		case schema.Public:
			return oldID
		case schema.Secret:
			return oldID + res.NbPublicVariables
		}
		return oldID
	}

	// we just need to offset our ids, such that wires = [ public wires  | secret wires | internal wires ]
	offsetIDs := func(l compiled.LinearExpression) {
		for j := 0; j < len(l); j++ {
			_, vID, visibility := l[j].Unpack()
			l[j].SetWireID(shiftVID(vID, visibility))
		}
	}

	for i := 0; i < len(res.Constraints); i++ {
		offsetIDs(res.Constraints[i].L.LinExp)
		offsetIDs(res.Constraints[i].R.LinExp)
		offsetIDs(res.Constraints[i].O.LinExp)
	}

	// we need to offset the ids in the hints
	shiftedMap := make(map[int]*compiled.Hint)

	// we need to offset the ids in the hints
HINTLOOP:
	for _, hint := range cs.MHints {
		ws := make([]int, len(hint.Wires))
		// we set for all outputs in shiftedMap. If one shifted output
		// is in shiftedMap, then all are
		for i, vID := range hint.Wires {
			ws[i] = shiftVID(vID, schema.Internal)
			if _, ok := shiftedMap[ws[i]]; i == 0 && ok {
				continue HINTLOOP
			}
		}
		inputs := make([]interface{}, len(hint.Inputs))
		copy(inputs, hint.Inputs)
		for j := 0; j < len(inputs); j++ {
			switch t := inputs[j].(type) {
			case compiled.Variable:
				tmp := make(compiled.LinearExpression, len(t.LinExp))
				copy(tmp, t.LinExp)
				offsetIDs(tmp)
				inputs[j] = tmp
			case compiled.LinearExpression:
				tmp := make(compiled.LinearExpression, len(t))
				copy(tmp, t)
				offsetIDs(tmp)
				inputs[j] = tmp
			default:
				inputs[j] = t
			}
		}
		ch := &compiled.Hint{ID: hint.ID, Inputs: inputs, Wires: ws}
		for _, vID := range ws {
			shiftedMap[vID] = ch
		}
	}
	res.MHints = shiftedMap

	// we need to offset the ids in Logs & DebugInfo
	for i := 0; i < len(cs.Logs); i++ {

		for j := 0; j < len(res.Logs[i].ToResolve); j++ {
			_, vID, visibility := res.Logs[i].ToResolve[j].Unpack()
			res.Logs[i].ToResolve[j].SetWireID(shiftVID(vID, visibility))
		}
	}
	for i := 0; i < len(cs.DebugInfo); i++ {
		for j := 0; j < len(res.DebugInfo[i].ToResolve); j++ {
			_, vID, visibility := res.DebugInfo[i].ToResolve[j].Unpack()
			res.DebugInfo[i].ToResolve[j].SetWireID(shiftVID(vID, visibility))
		}
	}

	// build levels
	res.Levels = buildLevels(res)

	switch cs.CurveID {
	case ecc.BLS12_377:
		return bls12377r1cs.NewR1CS(res, cs.st.Coeffs), nil
	case ecc.BLS12_381:
		return bls12381r1cs.NewR1CS(res, cs.st.Coeffs), nil
	case ecc.BN254:
		return bn254r1cs.NewR1CS(res, cs.st.Coeffs), nil
	case ecc.BW6_761:
		return bw6761r1cs.NewR1CS(res, cs.st.Coeffs), nil
	case ecc.BW6_633:
		return bw6633r1cs.NewR1CS(res, cs.st.Coeffs), nil
	case ecc.BLS24_315:
		return bls24315r1cs.NewR1CS(res, cs.st.Coeffs), nil
	default:
		panic("not implemtented")
	}
}

func (cs *compiler) SetSchema(s *schema.Schema) {
	if cs.Schema != nil {
		panic("SetSchema called multiple times")
	}
	cs.Schema = s
}

func buildLevels(ccs compiled.R1CS) [][]int {

	b := levelBuilder{
		mWireToNode: make(map[int]int, ccs.NbInternalVariables), // at which node we resolved which wire
		nodeLevels:  make([]int, len(ccs.Constraints)),          // level of a node
		mLevels:     make(map[int]int),                          // level counts
		ccs:         ccs,
		nbInputs:    ccs.NbPublicVariables + ccs.NbSecretVariables,
	}

	// for each constraint, we're going to find its direct dependencies
	// that is, wires (solved by previous constraints) on which it depends
	// each of these dependencies is tagged with a level
	// current constraint will be tagged with max(level) + 1
	for cID, c := range ccs.Constraints {

		b.nodeLevel = 0

		b.processLE(c.L.LinExp, cID)
		b.processLE(c.R.LinExp, cID)
		b.processLE(c.O.LinExp, cID)
		b.nodeLevels[cID] = b.nodeLevel
		b.mLevels[b.nodeLevel]++

	}

	levels := make([][]int, len(b.mLevels))
	for i := 0; i < len(levels); i++ {
		// allocate memory
		levels[i] = make([]int, 0, b.mLevels[i])
	}

	for n, l := range b.nodeLevels {
		levels[l] = append(levels[l], n)
	}

	return levels
}

type levelBuilder struct {
	ccs      compiled.R1CS
	nbInputs int

	mWireToNode map[int]int // at which node we resolved which wire
	nodeLevels  []int       // level per node
	mLevels     map[int]int // number of constraint per level

	nodeLevel int // current level
}

func (b *levelBuilder) processLE(l compiled.LinearExpression, cID int) {

	for _, t := range l {
		wID := t.WireID()
		if wID < b.nbInputs {
			// it's a input, we ignore it
			continue
		}

		// if we know a which constraint solves this wire, then it's a dependency
		n, ok := b.mWireToNode[wID]
		if ok {
			if n != cID { // can happen with hints...
				// we add a dependency, check if we need to increment our current level
				if b.nodeLevels[n] >= b.nodeLevel {
					b.nodeLevel = b.nodeLevels[n] + 1 // we are at the next level at least since we depend on it
				}
			}
			continue
		}

		// check if it's a hint and mark all the output wires
		if h, ok := b.ccs.MHints[wID]; ok {

			for _, in := range h.Inputs {
				switch t := in.(type) {
				case compiled.Variable:
					b.processLE(t.LinExp, cID)
				case compiled.LinearExpression:
					b.processLE(t, cID)
				case compiled.Term:
					b.processLE(compiled.LinearExpression{t}, cID)
				}
			}

			for _, hwid := range h.Wires {
				b.mWireToNode[hwid] = cID
			}
			continue
		}

		// mark this wire solved by current node
		b.mWireToNode[wID] = cID
	}
}

// ConstantValue returns the big.Int value of v.
// Will panic if v.IsConstant() == false
func (system *compiler) ConstantValue(v frontend.Variable) (*big.Int, bool) {
	if _v, ok := v.(compiled.Variable); ok {
		_v.AssertIsSet()

		if len(_v.LinExp) != 1 {
			return nil, false
		}
		cID, vID, visibility := _v.LinExp[0].Unpack()
		if !(vID == 0 && visibility == schema.Public) {
			return nil, false
		}
		return new(big.Int).Set(&system.st.Coeffs[cID]), true
	}
	r := utils.FromInterface(v)
	return &r, true
}

func (system *compiler) Backend() backend.ID {
	return backend.GROTH16
}

// toVariable will return (and allocate if neccesary) a compiled.Variable from given value
//
// if input is already a compiled.Variable, does nothing
// else, attempts to convert input to a big.Int (see utils.FromInterface) and returns a toVariable compiled.Variable
func (system *compiler) toVariable(input interface{}) frontend.Variable {

	switch t := input.(type) {
	case compiled.Variable:
		t.AssertIsSet()
		return t
	default:
		n := utils.FromInterface(t)
		if n.IsUint64() && n.Uint64() == 1 {
			return system.one()
		}
		r := system.one()
		r.LinExp[0] = system.setCoeff(r.LinExp[0], &n)
		return r
	}
}

// toVariables return frontend.Variable corresponding to inputs and the total size of the linear expressions
func (system *compiler) toVariables(in ...frontend.Variable) ([]compiled.Variable, int) {
	r := make([]compiled.Variable, 0, len(in))
	s := 0
	e := func(i frontend.Variable) {
		v := system.toVariable(i).(compiled.Variable)
		r = append(r, v)
		s += len(v.LinExp)
	}
	// e(i1)
	// e(i2)
	for i := 0; i < len(in); i++ {
		e(in[i])
	}
	return r, s
}

// Tag creates a tag at a given place in a circuit. The state of the tag may contain informations needed to
// measure constraints, variables and coefficients creations through AddCounter
func (system *compiler) Tag(name string) frontend.Tag {
	_, file, line, _ := runtime.Caller(1)

	return frontend.Tag{
		Name: fmt.Sprintf("%s[%s:%d]", name, filepath.Base(file), line),
		VID:  system.NbInternalVariables,
		CID:  len(system.Constraints),
	}
}

// AddCounter measures the number of constraints, variables and coefficients created between two tags
func (system *compiler) AddCounter(from, to frontend.Tag) {
	system.Counters = append(system.Counters, compiled.Counter{
		From:          from.Name,
		To:            to.Name,
		NbVariables:   to.VID - from.VID,
		NbConstraints: to.CID - from.CID,
		CurveID:       system.CurveID,
		BackendID:     backend.GROTH16,
	})
}

// NewHint initializes internal variables whose value will be evaluated using
// the provided hint function at run time from the inputs. Inputs must be either
// variables or convertible to *big.Int. The function returns an error if the
// number of inputs is not compatible with f.
//
// The hint function is provided at the proof creation time and is not embedded
// into the circuit. From the backend point of view, the variable returned by
// the hint function is equivalent to the user-supplied witness, but its actual
// value is assigned by the solver, not the caller.
//
// No new constraints are added to the newly created wire and must be added
// manually in the circuit. Failing to do so leads to solver failure.
func (system *compiler) NewHint(f hint.Function, nbOutputs int, inputs ...frontend.Variable) ([]frontend.Variable, error) {

	if nbOutputs <= 0 {
		return nil, fmt.Errorf("hint function must return at least one output")
	}
	hintInputs := make([]interface{}, len(inputs))

	// ensure inputs are set and pack them in a []uint64
	for i, in := range inputs {
		switch t := in.(type) {
		case compiled.Variable:
			tmp := t.Clone()
			hintInputs[i] = tmp.LinExp
		case compiled.LinearExpression:
			tmp := make(compiled.LinearExpression, len(t))
			copy(tmp, t)
			hintInputs[i] = tmp
		default:
			hintInputs[i] = utils.FromInterface(t)
		}
	}

	// prepare wires
	varIDs := make([]int, nbOutputs)
	res := make([]frontend.Variable, len(varIDs))
	for i := range varIDs {
		r := system.newInternalVariable()
		_, vID, _ := r.LinExp[0].Unpack()
		varIDs[i] = vID
		res[i] = r
	}

	ch := &compiled.Hint{ID: f.UUID(), Inputs: hintInputs, Wires: varIDs}
	for _, vID := range varIDs {
		system.MHints[vID] = ch
	}

	return res, nil
}
