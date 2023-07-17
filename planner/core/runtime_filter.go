// Copyright 2023 PingCAP, Inc.
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

package core

import (
	"fmt"
	"strings"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

// RuntimeFilterType "IN"
type RuntimeFilterType = variable.RuntimeFilterType

// RuntimeFilterMode "OFF", "LOCAL"
type RuntimeFilterMode = variable.RuntimeFilterMode

// RuntimeFilter structure is generated by the Runtime Filter Generator.
// At present, it has a one-to-one correspondence with the equivalent expression in HashJoin.
// For example:
// Query: select * from t1, t2 where t1.k1=t2.k1
// PhysicalPlanTree:
//
//	HashJoin_2(t1.k2=t2.k1)
//	/           \
//
// TableScan_0(t1)     ExchangeNode_1
// RuntimeFilter struct:
//
//	id: 0
//	buildNode: HashJoin
//	srcExprList: [t2.k1]
//	targetExprList: [t1.k1]
//	rfType:  IN
//	rfMode: LOCAL
//	buildNodeID: 2
//	targetNodeID: 0
//
// Although srcExprList and targetExprList attributes are lists, in fact, there will only be one value in them at present.
// The reason why it is designed as a list is because:
// Reserve an interface for subsequent multiple join equivalent expressions corresponding to one rf.
// (Since IN supports multiple columns, this construction can be more efficient at the execution level)
type RuntimeFilter struct {
	// runtime filter id, unique in one query plan
	id             int
	buildNode      *PhysicalHashJoin
	srcExprList    []*expression.Column
	targetExprList []*expression.Column
	rfType         RuntimeFilterType
	// The following properties need to be set after assigning a scan node to RF
	rfMode     RuntimeFilterMode
	targetNode *PhysicalTableScan
	// The plan id will be set when runtime filter clone()
	// It is only used for runtime filter pb
	buildNodeID  int
	targetNodeID int
}

// NewRuntimeFilter construct runtime filter by Join and equal predicate of Join
func NewRuntimeFilter(rfIDGenerator *util.IDGenerator, eqPredicate *expression.ScalarFunction, buildNode *PhysicalHashJoin) ([]*RuntimeFilter, int64) {
	rightSideIsBuild := buildNode.RightIsBuildSide()
	var srcExprList []*expression.Column
	var targetExprUniqueID int64
	if rightSideIsBuild {
		srcExprList = append(srcExprList, eqPredicate.GetArgs()[1].(*expression.Column))
		targetExprUniqueID = eqPredicate.GetArgs()[0].(*expression.Column).UniqueID
	} else {
		srcExprList = append(srcExprList, eqPredicate.GetArgs()[0].(*expression.Column))
		targetExprUniqueID = eqPredicate.GetArgs()[1].(*expression.Column).UniqueID
	}

	rfTypes := buildNode.ctx.GetSessionVars().GetRuntimeFilterTypes()
	result := make([]*RuntimeFilter, 0, len(rfTypes))
	for _, rfType := range rfTypes {
		rf := &RuntimeFilter{
			id:          rfIDGenerator.GetNextID(),
			buildNode:   buildNode,
			srcExprList: srcExprList,
			rfType:      rfType,
		}
		result = append(result, rf)
	}
	return result, targetExprUniqueID
}

func (rf *RuntimeFilter) assign(targetNode *PhysicalTableScan, targetExpr *expression.Column) {
	rf.targetNode = targetNode
	if len(rf.targetNode.runtimeFilterList) == 0 {
		// todo use session variables instead
		rf.targetNode.maxWaitTimeMs = 10000
	}
	rf.targetExprList = append(rf.targetExprList, targetExpr)
	rf.buildNode.runtimeFilterList = append(rf.buildNode.runtimeFilterList, rf)
	rf.targetNode.runtimeFilterList = append(rf.targetNode.runtimeFilterList, rf)
	logutil.BgLogger().Debug("Assign RF to target node",
		zap.String("RuntimeFilter", rf.String()))
}

// ExplainInfo explain info of runtime filter
func (rf *RuntimeFilter) ExplainInfo(isBuildNode bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d[%s]", rf.id, rf.rfType)
	if isBuildNode {
		fmt.Fprintf(&builder, " <- ")
		for i, srcExpr := range rf.srcExprList {
			if i != 0 {
				fmt.Fprintf(&builder, ",")
			}
			fmt.Fprintf(&builder, "%s", srcExpr.String())
		}
	} else {
		fmt.Fprintf(&builder, " -> ")
		for i, targetExpr := range rf.targetExprList {
			if i != 0 {
				fmt.Fprintf(&builder, ",")
			}
			fmt.Fprintf(&builder, "%s", targetExpr.String())
		}
	}
	return builder.String()
}

func (rf *RuntimeFilter) String() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "id=%d", rf.id)
	builder.WriteString(", ")
	fmt.Fprintf(&builder, "buildNodeID=%d", rf.buildNode.id)
	builder.WriteString(", ")
	if rf.targetNode == nil {
		fmt.Fprintf(&builder, "targetNodeID=nil")
	} else {
		fmt.Fprintf(&builder, "targetNodeID=%d", rf.targetNode.id)
	}
	builder.WriteString(", ")
	fmt.Fprintf(&builder, "srcColumn=")
	for _, srcExpr := range rf.srcExprList {
		fmt.Fprintf(&builder, "%s,", srcExpr.String())
	}
	builder.WriteString(", ")
	fmt.Fprintf(&builder, "targetColumn=")
	for _, targetExpr := range rf.targetExprList {
		fmt.Fprintf(&builder, "%s,", targetExpr.String())
	}
	builder.WriteString(", ")
	fmt.Fprintf(&builder, "rfType=%s", rf.rfType)
	builder.WriteString(", ")
	if rf.rfMode == 0 {
		fmt.Fprintf(&builder, "rfMode=nil")
	} else {
		fmt.Fprintf(&builder, "rfMode=%s", rf.rfMode)
	}
	builder.WriteString(".")
	return builder.String()
}

// Clone deep copy of runtime filter
func (rf *RuntimeFilter) Clone() *RuntimeFilter {
	cloned := new(RuntimeFilter)
	cloned.id = rf.id
	// Because build node only needs to get its executor id attribute when converting to pb format,
	// so we only copy explain id here
	if rf.buildNode == nil {
		cloned.buildNodeID = rf.buildNodeID
	} else {
		cloned.buildNodeID = rf.buildNode.id
	}
	if rf.targetNode == nil {
		cloned.targetNodeID = rf.targetNodeID
	} else {
		cloned.targetNodeID = rf.targetNode.id
	}

	for _, srcExpr := range rf.srcExprList {
		cloned.srcExprList = append(cloned.srcExprList, srcExpr.Clone().(*expression.Column))
	}
	for _, targetExpr := range rf.targetExprList {
		cloned.targetExprList = append(cloned.targetExprList, targetExpr.Clone().(*expression.Column))
	}
	cloned.rfType = rf.rfType
	cloned.rfMode = rf.rfMode
	return cloned
}

// RuntimeFilterListToPB convert runtime filter list to PB list
func RuntimeFilterListToPB(runtimeFilterList []*RuntimeFilter, sc *stmtctx.StatementContext, client kv.Client) ([]*tipb.RuntimeFilter, error) {
	result := make([]*tipb.RuntimeFilter, 0, len(runtimeFilterList))
	for _, runtimeFilter := range runtimeFilterList {
		rfPB, err := runtimeFilter.ToPB(sc, client)
		if err != nil {
			return nil, err
		}
		result = append(result, rfPB)
	}
	return result, nil
}

// ToPB convert runtime filter to PB
func (rf *RuntimeFilter) ToPB(sc *stmtctx.StatementContext, client kv.Client) (*tipb.RuntimeFilter, error) {
	pc := expression.NewPBConverter(client, sc)
	srcExprListPB := make([]*tipb.Expr, 0, len(rf.srcExprList))
	for _, srcExpr := range rf.srcExprList {
		srcExprPB := pc.ExprToPB(srcExpr)
		if srcExprPB == nil {
			return nil, ErrInternal.GenWithStack("failed to transform src expr %s to pb in runtime filter", srcExpr.String())
		}
		srcExprListPB = append(srcExprListPB, srcExprPB)
	}
	targetExprListPB := make([]*tipb.Expr, 0, len(rf.targetExprList))
	for _, targetExpr := range rf.targetExprList {
		targetExprPB := pc.ExprToPB(targetExpr)
		if targetExprPB == nil {
			return nil, ErrInternal.GenWithStack("failed to transform target expr %s to pb in runtime filter", targetExpr.String())
		}
		targetExprListPB = append(targetExprListPB, targetExprPB)
	}
	rfTypePB := tipb.RuntimeFilterType_IN
	switch rf.rfType {
	case variable.In:
		rfTypePB = tipb.RuntimeFilterType_IN
	case variable.MinMax:
		rfTypePB = tipb.RuntimeFilterType_MIN_MAX
	}
	rfModePB := tipb.RuntimeFilterMode_LOCAL
	switch rf.rfMode {
	case variable.RFLocal:
		rfModePB = tipb.RuntimeFilterMode_LOCAL
	case variable.RFGlobal:
		rfModePB = tipb.RuntimeFilterMode_GLOBAL
	}
	result := &tipb.RuntimeFilter{
		Id:               int32(rf.id),
		SourceExprList:   srcExprListPB,
		TargetExprList:   targetExprListPB,
		SourceExecutorId: fmt.Sprint(rf.buildNodeID),
		TargetExecutorId: fmt.Sprint(rf.targetNodeID),
		RfType:           rfTypePB,
		RfMode:           rfModePB,
	}
	return result, nil
}
