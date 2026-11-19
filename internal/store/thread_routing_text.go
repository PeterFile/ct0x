package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxThreadTextBytes = 4096
	maxThreadTitles    = 128
)

var (
	threadNewSubject = regexp.MustCompile(`(?i)^(?:(?:开(?:个|一个)?|开启|新建)?[ \t]*新(?:的)?(?:议题|话题)|(?:let's |start )?(?:a )?new (?:topic|discussion))[ \t]*[:：][ \t]*`)
	threadInvite     = regexp.MustCompile(`(?i)^(?:(?:我们|咱们)(?:来|先)?讨论|let['’]s discuss[ \t]+)`)
	threadBusiness   = regexp.MustCompile(`(?i)(?:规划|提案|报告|策略|设计|方案|复盘|验证|调查|审查|排期|迁移|预算|改进|回顾|项目|告警|故障|事故|\b(?:plan|proposal|report|strategy|design|review|migration|budget|investigation|project|alert|incident|bug))$`)
	threadCurrent    = regexp.MustCompile(`(?i)^(?:(?:当前|现在|这次|本次)(?:只)?(?:讨论|处理|跟进)|(?:请)?(?:跟进|继续处理)(?:那个)?|current (?:subject|topic)|follow up on)[ \t]*[:：]?\s*`)
	threadComparison = regexp.MustCompile(`(?i)(?:仅作(?:对照|比较|参考)|(?:只)?作为(?:对照|比较|参考)|仅供参考|for comparison|comparison only)`)
	threadIssue      = regexp.MustCompile(`(?i)[a-z][a-z0-9]{1,15}-[0-9]{1,12}`)
	threadNonName    = regexp.MustCompile(`(?:什么|怎么样|怎么|如何|吗|一下|随便|闲聊|还好吗|可以吗|是否|^这个|^那个|^这个事|^那件事|^the (?:same|other) thing$)`)
	threadGeneric    = regexp.MustCompile(`(?i)^(?:(?:议题|话题|任务|工作|项目|问题|进展|告警|故障|事故|复盘|规划|计划|报告|方案|topic|issue|task|work|project|status|alert|incident|bug|plan|report|review)[ \t/_-]*)+$`)
)

type threadTopicEvidence struct {
	topic             string
	conflict, matched bool
}

func (e *threadTopicEvidence) add(topic string) {
	if topic != "" {
		e.confirm(topic)
	}
}

func (e *threadTopicEvidence) confirm(topic string) {
	e.conflict = e.conflict || (e.matched && e.topic != topic)
	e.topic, e.matched = topic, true
}

type threadTextRoute struct {
	threadTopicEvidence
	current, comparison bool
	newTitle, newIssue  string
}

func resolveThreadTextInTx(ctx context.Context, tx *sql.Tx, group, body string) (threadTextRoute, error) {
	var route threadTextRoute
	if !validHistoryText(body, maxThreadTextBytes) {
		return route, nil
	}
	active := unquotedThreadText(body)
	clauses := strings.FieldsFunc(active, func(r rune) bool { return strings.ContainsRune("\n。！；，;!,", r) })
	var title, exact string
	var openingFollowup bool
	var aliases map[string]struct{}
	for i, clause := range clauses {
		clause = strings.TrimSpace(clause)
		switch {
		case i == 0 && threadNewSubject.MatchString(clause):
			route.current = true
			title = threadName(threadNewSubject.ReplaceAllString(clause, ""))
		case i == 0 && threadInvite.MatchString(clause):
			route.current = true
			title = threadName(threadInvite.ReplaceAllString(clause, ""))
			if !threadBusiness.MatchString(title) && len(issueTokens(title)) != 1 {
				title = ""
			}
		case threadCurrent.MatchString(clause):
			if route.current && title == "" {
				route.conflict = true
				return route, nil
			}
			route.current = true
			subject := threadName(threadCurrent.ReplaceAllString(clause, ""))
			if title == "" {
				exact = subject
			} else {
				// An action prefix after a declared title is not a second
				// subject unless the remaining text supplies evidence.
				openingFollowup = true
			}
			if subject != "" && len(issueTokens(subject)) == 0 {
				if aliases == nil {
					aliases = make(map[string]struct{})
				}
				aliases[threadTitleKey(subject)] = struct{}{}
			}
		case threadComparison.MatchString(clause):
			route.comparison = true
			clauses[i] = ""
		}
	}
	if route.comparison {
		active = strings.Join(clauses, "\n")
	}
	if title != "" {
		exact = title
	}
	titleIssues := issueTokens(title)
	if exact != "" {
		var topic string
		err := tx.QueryRowContext(ctx, `SELECT topic_id FROM main.thread_routing_topics
		 WHERE group_id=? AND title_key=?`, group, threadTitleKey(exact)).Scan(&topic)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return route, err
		}
		route.add(topic)
	}
	issues := issueTokens(active)
	if len(issues) > 1 {
		route.conflict = true
		return route, nil
	}
	if len(issues) == 1 {
		var topic string
		err := tx.QueryRowContext(ctx, `SELECT topic_id FROM main.thread_routing_topics
		 WHERE group_id=? AND issue_id=? AND issue_id<>''`, group, issues[0]).Scan(&topic)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return route, err
		}
		if openingFollowup && (len(titleIssues) == 0 || titleIssues[0] != issues[0]) && (topic == "" || topic != route.topic) {
			route.conflict = true
		}
		route.add(topic)
	}
	if active == "" {
		return route, nil
	}
	names, complete, err := threadTitleEvidenceInTx(ctx, tx, group, active, aliases)
	if err != nil {
		return route, err
	}
	// A known name elsewhere in the message cannot replace an unmatched
	// declared title. In an incomplete catalog it may be unsafe to decide.
	if openingFollowup && (!complete || (!route.matched && names.matched)) {
		route.conflict = true
	}
	route.add(names.topic)
	route.conflict = route.conflict || names.conflict
	if title != "" && !route.conflict && route.topic == "" && complete {
		var issue string
		if len(titleIssues) == 1 {
			issue = titleIssues[0]
		}
		route.newTitle, route.newIssue = title, issue
	}
	return route, nil
}

func threadTitleEvidenceInTx(ctx context.Context, tx *sql.Tx, group, active string, aliases map[string]struct{}) (threadTopicEvidence, bool, error) {
	var evidence threadTopicEvidence
	rows, err := tx.QueryContext(ctx, `SELECT topic_id,title_key,issue_id FROM main.thread_routing_topics
	 WHERE group_id=? ORDER BY topic_id LIMIT ?`, group, maxThreadTitles+1)
	if err != nil {
		return evidence, false, err
	}
	defer rows.Close()
	text := threadTitleKey(active)
	for n := 0; rows.Next(); n++ {
		if n == maxThreadTitles {
			// Never infer a unique name from an incomplete sample. The issue ID
			// and structured references do not depend on catalog order or age.
			return threadTopicEvidence{}, false, nil
		}
		var topic, key, issue string
		if err := rows.Scan(&topic, &key, &issue); err != nil {
			return evidence, false, err
		}
		var aliasMatch bool
		if len(aliases) != 0 {
			_, aliasMatch = aliases[threadIssueAlias(key, issue)]
		}
		if aliasMatch || (!threadGeneric.MatchString(key) && hasThreadToken(text, key, isNonHanWord)) {
			evidence.add(topic)
		}
	}
	return evidence, true, rows.Err()
}

// Remove only the separately recognized issue ID, at one end of the name.
// The remaining text must independently qualify as a name.
func threadIssueAlias(key, issue string) string {
	if issue == "" {
		return ""
	}
	const delimiters = " :：–—()（）[]【】"
	key = strings.Trim(key, delimiters)
	issue = strings.ToLower(issue)
	var name string
	switch {
	case strings.HasPrefix(key, issue) && threadTokenBounds(key, 0, len(issue), isIssueWord):
		name = strings.TrimLeft(key[len(issue):], delimiters)
	case strings.HasSuffix(key, issue) && threadTokenBounds(key, len(key)-len(issue), len(key), isIssueWord):
		name = strings.TrimRight(key[:len(key)-len(issue)], delimiters)
	}
	return threadTitleKey(threadName(name))
}

func threadTitleKey(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func threadName(s string) string {
	// A creation title is one short named subject, not an entire prose body.
	for i, r := range s {
		if r == '?' || r == '？' {
			return ""
		}
		if strings.ContainsRune("\n。！；，、;!,", r) || r == '.' && (i+1 == len(s) || s[i+1] == ' ') {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(s)
	if !validHistoryText(s, 192) || threadNonName.MatchString(s) || threadGeneric.MatchString(s) {
		return ""
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			letters++
		}
	}
	if letters < 4 {
		return ""
	}
	return s
}

func issueTokens(text string) []string {
	var ids []string
	for _, match := range threadIssue.FindAllStringIndex(text, -1) {
		if !threadTokenBounds(text, match[0], match[1], isIssueWord) {
			continue
		}
		id := strings.ToUpper(text[match[0]:match[1]])
		if len(ids) == 0 || id != ids[0] {
			ids = append(ids, id)
			if len(ids) == 2 {
				break
			}
		}
	}
	return ids
}

func hasThreadToken(text, token string, word func(rune) bool) bool {
	if token == "" {
		return false
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], token)
		if index == -1 {
			return false
		}
		start := offset + index
		if threadTokenBounds(text, start, start+len(token), word) {
			return true
		}
		offset = start + 1
	}
	return false
}

func threadTokenBounds(text string, start, end int, word func(rune) bool) bool {
	left, _ := utf8.DecodeLastRuneInString(text[:start])
	right, _ := utf8.DecodeRuneInString(text[end:])
	first, _ := utf8.DecodeRuneInString(text[start:end])
	last, _ := utf8.DecodeLastRuneInString(text[start:end])
	return !(word(left) && word(first) || word(right) && word(last))
}

func isNonHanWord(r rune) bool {
	return !unicode.Is(unicode.Han, r) && (unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_')
}

func isIssueWord(r rune) bool { return isNonHanWord(r) || r == '-' }

var threadQuotes = strings.NewReplacer("“", "\x01", "”", "\x01", "「", "\x02", "」", "\x02", "『", "\x03", "』", "\x03")

func unquotedThreadText(body string) string {
	var active strings.Builder
	var fence string
	var quoted rune
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			fence = line[:3]
			continue
		case strings.HasPrefix(line, ">") || strings.HasPrefix(line, "＞"):
			continue
		}
		for _, r := range threadQuotes.Replace(line) {
			if quoted != 0 {
				if r == quoted {
					quoted = 0
				}
				continue
			}
			switch r {
			case '"', '`', '\x01', '\x02', '\x03':
				quoted = r
				active.WriteByte(' ')
			default:
				active.WriteRune(r)
			}
		}
		active.WriteByte('\n')
	}
	return strings.TrimSpace(active.String())
}
