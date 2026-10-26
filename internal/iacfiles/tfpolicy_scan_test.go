package iacfiles

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file reads IAM policy statements out of Terraform source without a
// Terraform or HCL dependency (the module has neither). It tokenizes the HCL
// native syntax far enough to know where strings, comments, heredocs and
// brackets begin and end, then treats every object constructor and block body
// that has an Action (jsonencode) or actions (aws_iam_policy_document) attribute
// as one statement. Nothing keys off a Sid: a Sid is optional free text, so a
// scan anchored on it never sees a statement written without one (#782).

type tfTokenKind int

const (
	tfPunct   tfTokenKind = iota // one of { } [ ] ( ) , = :
	tfString                     // quoted string or heredoc; text is the content between the delimiters
	tfWord                       // identifier, number, traversal or operator
	tfNewline                    // attribute separator in HCL object and block bodies
)

type tfToken struct {
	kind tfTokenKind
	text string
}

type tfAttribute struct {
	key   string
	value []tfToken
}

const tfPunctChars = "{}[](),=:"

// tfActionKey counts the Action keys in the raw text, independently of the
// tokenizer, so a statement the tokenizer cannot see (inside a heredoc, say)
// shows up as a count mismatch instead of passing silently. It is
// case-insensitive and also matches the plural `actions` of an
// aws_iam_policy_document statement block; `NotAction` and `not_actions` are
// excluded by the leading non-word character.
var tfActionKey = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])"?actions?"?\s*[=:]`)

// tfStatements returns every IAM policy statement in a Terraform file.
func tfStatements(t *testing.T, raw []byte) []awsPolicyStatement {
	t.Helper()
	statements, err := parseTFStatements(raw)
	require.NoError(t, err)
	return statements
}

// parseTFStatements returns every IAM policy statement in a Terraform file. It
// fails rather than guess when a statement's Action or Resource is not a list
// of literal strings, and when the number of statements it found differs from
// the number of Action keys in the text.
func parseTFStatements(raw []byte) ([]awsPolicyStatement, error) {
	toks, err := tfTokenize(string(raw))
	if err != nil {
		return nil, err
	}
	var out []awsPolicyStatement
	for i, tok := range toks {
		if tok.kind != tfPunct || tok.text != "{" {
			continue
		}
		stmt, ok, err := tfStatementFrom(tfAttributes(toks, i+1))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, stmt)
		}
	}
	if keys := len(tfActionKey.FindAllString(string(raw), -1)); keys != len(out) {
		return nil, fmt.Errorf("found %d Action keys in the text but parsed %d statements; a statement this scan cannot see would pass unchecked", keys, len(out))
	}
	return out, nil
}

// tfStatementFrom builds a statement from an object's attributes. ok is false
// when the object has no Action attribute and so is not a statement.
func tfStatementFrom(attrs []tfAttribute) (stmt awsPolicyStatement, ok bool, err error) {
	var actions, resources int
	for _, attr := range attrs {
		key := strings.ToLower(attr.key)
		switch key {
		case "action", "actions":
			actions++
			stmt.Action, err = tfLiteralStrings(attr)
		case "resource", "resources":
			resources++
			var list []string
			list, err = tfLiteralStrings(attr)
			// A multi-resource list is joined so it never equals a single ARN.
			stmt.Resource = strings.Join(list, ",")
		}
		if err != nil {
			return stmt, false, err
		}
	}
	if actions > 1 || resources > 1 {
		return stmt, false, fmt.Errorf("statement %v repeats its Action or Resource key", stmt.Action)
	}
	return stmt, actions == 1, nil
}

// tfLiteralStrings returns the strings of a value that is one literal string or
// a list of them. Anything else (a reference, a function call, an
// interpolation) is an error: the test cannot know what it resolves to.
func tfLiteralStrings(attr tfAttribute) ([]string, error) {
	var out []string
	for _, tok := range attr.value {
		switch {
		case tok.kind == tfString && !strings.Contains(tok.text, "${") && !strings.Contains(tok.text, "%{"):
			out = append(out, tok.text)
		case tok.kind == tfNewline, tok.kind == tfPunct && strings.Contains("[],", tok.text):
			// List brackets and separators carry no value.
		default:
			return nil, fmt.Errorf("%s holds %q, which is not a literal string; write it as literal strings so this test can check it", attr.key, tok.text)
		}
	}
	return out, nil
}

// tfAttributes returns the `key = value` and `key: value` attributes of the
// object or block body that starts at toks[i], up to its closing brace. Nested
// blocks and anything else that is not an attribute are skipped.
func tfAttributes(toks []tfToken, i int) []tfAttribute {
	var attrs []tfAttribute
	for i < len(toks) {
		tok := toks[i]
		switch {
		case tfIsClose(tok):
			return attrs
		case tok.kind == tfNewline, tok.kind == tfPunct && tok.text == ",":
			i++
		case (tok.kind == tfWord || tok.kind == tfString) && i+1 < len(toks) &&
			toks[i+1].kind == tfPunct && (toks[i+1].text == "=" || toks[i+1].text == ":"):
			end := tfValueEnd(toks, i+2)
			attrs = append(attrs, tfAttribute{key: tok.text, value: toks[i+2 : end]})
			i = end
		default:
			i = tfValueEnd(toks, i+1)
		}
	}
	return attrs
}

// tfValueEnd returns the index of the newline, comma or closing bracket that
// ends the value starting at toks[i], ignoring those inside nested brackets.
func tfValueEnd(toks []tfToken, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tfIsOpen(tok):
			depth++
		case tfIsClose(tok):
			if depth == 0 {
				return i
			}
			depth--
		case depth == 0 && (tok.kind == tfNewline || tok.kind == tfPunct && tok.text == ","):
			return i
		}
	}
	return i
}

func tfIsOpen(tok tfToken) bool {
	return tok.kind == tfPunct && (tok.text == "{" || tok.text == "[" || tok.text == "(")
}

func tfIsClose(tok tfToken) bool {
	return tok.kind == tfPunct && (tok.text == "}" || tok.text == "]" || tok.text == ")")
}

// tfTokenize splits Terraform source into tokens, dropping comments and
// whitespace other than newlines.
func tfTokenize(src string) ([]tfToken, error) {
	var toks []tfToken
	for i := 0; i < len(src); {
		tok, next, err := tfNextToken(src, i)
		if err != nil {
			return nil, err
		}
		if tok != nil {
			toks = append(toks, *tok)
		}
		i = next
	}
	return toks, nil
}

// tfNextToken reads the token at src[i]. It returns a nil token for
// whitespace and comments.
func tfNextToken(src string, i int) (tok *tfToken, next int, err error) {
	c := src[i]
	switch {
	case c == ' ' || c == '\t' || c == '\r':
		return nil, i + 1, nil
	case c == '\n':
		return &tfToken{kind: tfNewline, text: "\n"}, i + 1, nil
	case c == '#' || strings.HasPrefix(src[i:], "//") || strings.HasPrefix(src[i:], "/*"):
		next, err := tfSkipComment(src, i)
		return nil, next, err
	case c == '"':
		end, err := tfSkipQuoted(src, i)
		if err != nil {
			return nil, 0, err
		}
		return &tfToken{kind: tfString, text: src[i+1 : end-1]}, end, nil
	case strings.HasPrefix(src[i:], "<<"):
		return tfHeredoc(src, i)
	case strings.IndexByte(tfPunctChars, c) >= 0:
		return &tfToken{kind: tfPunct, text: string(c)}, i + 1, nil
	}
	end := tfWordEnd(src, i)
	return &tfToken{kind: tfWord, text: src[i:end]}, end, nil
}

// tfWordEnd returns the index just past the word that starts at src[i].
func tfWordEnd(src string, i int) int {
	end := i + 1
	for end < len(src) && !strings.ContainsRune(" \t\r\n\"#"+tfPunctChars, rune(src[end])) &&
		!strings.HasPrefix(src[end:], "//") && !strings.HasPrefix(src[end:], "/*") {
		end++
	}
	return end
}

// tfSkipComment returns the index just past the comment that starts at
// src[i]; a line comment ends before its newline, which is still a token.
func tfSkipComment(src string, i int) (int, error) {
	if !strings.HasPrefix(src[i:], "/*") {
		return tfLineEnd(src, i), nil
	}
	end := strings.Index(src[i+2:], "*/")
	if end < 0 {
		return 0, fmt.Errorf("unterminated comment at offset %d", i)
	}
	return i + 2 + end + 2, nil
}

func tfLineEnd(src string, i int) int {
	if n := strings.IndexByte(src[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(src)
}

// tfSkipQuoted returns the index just past the closing quote of the quoted
// string that opens at src[i], stepping over escapes and over `${...}` and
// `%{...}` template sequences, which may themselves contain quoted strings.
func tfSkipQuoted(src string, i int) (int, error) {
	for j := i + 1; j < len(src); j++ {
		switch {
		case src[j] == '\\':
			j++
		case src[j] == '"':
			return j + 1, nil
		case src[j] == '\n':
			return 0, fmt.Errorf("unterminated string at offset %d", i)
		case strings.HasPrefix(src[j:], "$${"), strings.HasPrefix(src[j:], "%%{"):
			j += 2 // escaped template sequence: literal text
		case strings.HasPrefix(src[j:], "${"), strings.HasPrefix(src[j:], "%{"):
			end, err := tfSkipTemplate(src, j+2)
			if err != nil {
				return 0, err
			}
			j = end - 1
		}
	}
	return 0, fmt.Errorf("unterminated string at offset %d", i)
}

// tfSkipTemplate returns the index just past the brace that closes a template
// sequence whose body starts at src[i].
func tfSkipTemplate(src string, i int) (int, error) {
	depth := 1
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '"':
			end, err := tfSkipQuoted(src, j)
			if err != nil {
				return 0, err
			}
			j = end - 1
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated template sequence at offset %d", i)
}

// tfHeredoc reads a `<<EOF` or `<<-EOF` heredoc as one string token ending
// just before the newline that follows its closing marker.
func tfHeredoc(src string, i int) (tok *tfToken, next int, err error) {
	header := tfLineEnd(src, i)
	marker := strings.TrimPrefix(strings.TrimPrefix(src[i:header], "<<"), "-")
	marker = strings.TrimSpace(marker)
	if marker == "" || header == len(src) {
		return nil, 0, fmt.Errorf("malformed heredoc at offset %d", i)
	}
	for line := header + 1; line < len(src); {
		end := tfLineEnd(src, line)
		if strings.TrimSpace(src[line:end]) == marker {
			return &tfToken{kind: tfString, text: src[header+1 : line]}, end, nil
		}
		line = end + 1
	}
	return nil, 0, fmt.Errorf("unterminated heredoc %q at offset %d", marker, i)
}
