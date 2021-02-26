package completer

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/hypnoce/sqls/ast"
	"github.com/hypnoce/sqls/ast/astutil"
	"github.com/hypnoce/sqls/database"
	"github.com/hypnoce/sqls/dialect"
	"github.com/hypnoce/sqls/lsp"
	"github.com/hypnoce/sqls/parser"
	"github.com/hypnoce/sqls/parser/parseutil"
	"github.com/hypnoce/sqls/token"
)

type completionType int

const (
	_ completionType = iota
	CompletionTypeKeyword
	CompletionTypeFunction
	CompletionTypeColumn
	CompletionTypeTable
	CompletionTypeReferencedTable
	CompletionTypeView
	CompletionTypeSubQuery
	CompletionTypeSubQueryColumn
	CompletionTypeChange
	CompletionTypeUser
	CompletionTypeSchema
)

func (ct completionType) String() string {
	switch ct {
	case CompletionTypeKeyword:
		return "Keyword"
	case CompletionTypeFunction:
		return "Function"
	case CompletionTypeColumn:
		return "Column"
	case CompletionTypeTable:
		return "Table"
	case CompletionTypeReferencedTable:
		return "ReferencedTable"
	case CompletionTypeView:
		return "View"
	case CompletionTypeChange:
		return "Change"
	case CompletionTypeUser:
		return "User"
	case CompletionTypeSchema:
		return "Schema"
	default:
		return ""
	}
}

type Completer struct {
	DBCache *database.DBCache
	Driver  dialect.DatabaseDriver
}

func NewCompleter(dbCache *database.DBCache) *Completer {
	return &Completer{
		DBCache: dbCache,
	}
}

func completionTypeIs(completionTypes []completionType, expect completionType) bool {
	for _, t := range completionTypes {
		if t == expect {
			return true
		}
	}
	return false
}

func (c *Completer) Complete(text string, params lsp.CompletionParams, lowercaseKeywords bool) ([]lsp.CompletionItem, error) {
	parsed, err := parser.Parse(text)
	if err != nil {
		return nil, err
	}

	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character,
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	ctx := getCompletionTypes(nodeWalker)
	if err != nil {
		return nil, err
	}

	definedTables, err := parseutil.ExtractTable(parsed, pos)
	if err != nil {
		return nil, err
	}
	definedSubQuerys, err := parseutil.ExtractSubQueryViews(parsed, pos)
	if err != nil {
		return nil, err
	}

	lastWord := getLastWord(text, params.Position.Line+1, params.Position.Character)
	withBackQuote := strings.HasPrefix(lastWord, "\"")

	items := []lsp.CompletionItem{}

	if c.DBCache != nil {
		if completionTypeIs(ctx.types, CompletionTypeColumn) {
			candidates := c.columnCandidates(definedTables, ctx.parent)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeReferencedTable) {
			candidates := c.ReferencedTableCandidates(definedTables)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeTable) {
			candidates := c.TableCandidates(ctx.parent, definedTables)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSchema) {
			candidates := c.SchemaCandidates()
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSubQuery) {
			candidates := c.SubQueryCandidates(definedSubQuerys)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(ctx.types, CompletionTypeSubQueryColumn) {
			candidates := c.SubQueryColumnCandidates(definedSubQuerys)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
	}

	if completionTypeIs(ctx.types, CompletionTypeKeyword) {
		drivers := dialect.DataBaseKeywords(c.Driver)
		items = append(items, c.keywordCandidates(lowercaseKeywords, drivers)...)
	}
	if completionTypeIs(ctx.types, CompletionTypeFunction) {
		drivers := dialect.DataBaseFunctions(c.Driver)
		items = append(items, c.functionCandidates(lowercaseKeywords, drivers)...)
	}

	items = filterCandidates(items, lastWord)

	return items, nil
}

type ParentType int

const (
	_ ParentType = iota
	ParentTypeNone
	ParentTypeSchema
	ParentTypeTable
	ParentTypeSubQuery
)

type completionParent struct {
	Type ParentType
	Name string
}

var noneParent = &completionParent{Type: ParentTypeNone}

type CompletionContext struct {
	types  []completionType
	parent *completionParent
}

func getCompletionTypes(nw *parseutil.NodeWalker) *CompletionContext {
	memberIdentifierMatcher := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeMemberIdentifer},
	}

	syntaxPos := parseutil.CheckSyntaxPosition(nw)
	t := []completionType{}
	p := noneParent
	switch {
	case syntaxPos == parseutil.ColName:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifer)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeSubQueryColumn,
				CompletionTypeView,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.Parent.String(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeView,
				CompletionTypeFunction,
				CompletionTypeKeyword,
			}
			p = noneParent
		}
	case syntaxPos == parseutil.AliasName:
		// pass
	case syntaxPos == parseutil.SelectExpr || syntaxPos == parseutil.CaseValue:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifer)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.ParentTok.NoQuateString(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeFunction,
				CompletionTypeKeyword,
			}
		}
	case syntaxPos == parseutil.TableReference:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifer)
			t = []completionType{
				CompletionTypeTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeSchema,
				Name: mi.ParentTok.NoQuateString(),
			}
		} else {
			t = []completionType{
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeSchema,
				CompletionTypeView,
				CompletionTypeSubQuery,
				CompletionTypeKeyword,
			}
		}
	case syntaxPos == parseutil.WhereCondition:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifer)
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeFunction,
			}
			p = &completionParent{
				Type: ParentTypeTable,
				Name: mi.ParentTok.NoQuateString(),
			}
		} else {
			t = []completionType{
				CompletionTypeColumn,
				CompletionTypeTable,
				CompletionTypeReferencedTable,
				CompletionTypeView,
				CompletionTypeSubQueryColumn,
				CompletionTypeSubQuery,
				CompletionTypeFunction,
				CompletionTypeKeyword,
			}
		}
	case syntaxPos == parseutil.InsertColumn:
		t = []completionType{
			CompletionTypeColumn,
			CompletionTypeView,
		}
	default:
		t = []completionType{
			CompletionTypeKeyword,
		}
	}
	return &CompletionContext{
		types:  t,
		parent: p,
	}
}

func filterCandidates(candidates []lsp.CompletionItem, lastWord string) []lsp.CompletionItem {
	filterd := []lsp.CompletionItem{}
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToUpper(candidate.Label), strings.ToUpper(lastWord)) {
			filterd = append(filterd, candidate)
		}
	}
	return filterd
}

func getLine(text string, line int) string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	i := 1
	for scanner.Scan() {
		if i == line {
			return scanner.Text()
		}
		i++
	}
	return ""
}

func getLastWord(text string, line, char int) string {
	t := getBeforeCursorText(text, line, char)
	s := getLine(t, line)

	reg := regexp.MustCompile("[\\w\"]+$")
	ss := reg.FindAllString(s, -1)
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}

func getBeforeCursorText(text string, line, char int) string {
	writer := bytes.NewBufferString("")
	scanner := bufio.NewScanner(strings.NewReader(text))

	i := 1
	for scanner.Scan() {
		if i == line {
			t := scanner.Text()
			writer.Write([]byte(t[:char]))
			break
		}
		writer.Write([]byte(fmt.Sprintln(scanner.Text())))
		i++
	}
	return writer.String()
}

func toQuotedCandidates(candidates []lsp.CompletionItem) []lsp.CompletionItem {
	quotedCandidates := make([]lsp.CompletionItem, len(candidates))
	for i, candidate := range candidates {
		candidate.Label = fmt.Sprintf("\"%s\"", candidate.Label)
		quotedCandidates[i] = candidate
	}
	return quotedCandidates
}
