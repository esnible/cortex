package inferenceparser

import "strings"

// dialect is the wire format an inference endpoint speaks, which decides how both its
// request and its response are read.
type dialect int

const (
	dialectNone      dialect = iota // not an endpoint this parser reads
	dialectOpenAI                   // OpenAI chat completions, or legacy completions
	dialectAnthropic                // Anthropic Messages
)

// anthropicMessagesPath is the Anthropic Messages endpoint, and the ending that marks the
// dialect under any prefix: Anthropic's own /v1/messages, OpenCode Zen's /zen/v1/messages
// and /zen/go/v1/messages, and the /anthropic/v1/messages that gateways mount. Clients
// such as Claude Code POST here instead of to /v1/chat/completions, so the parser must
// recognise both dialects.
const anthropicMessagesPath = "/v1/messages"

// completionsSuffix ends every OpenAI-dialect inference path: /v1/chat/completions and the
// legacy /v1/completions, at the root or under any prefix — IBM Bob's /inference, OpenCode
// Zen's /zen and /zen/go, OpenRouter's /api, Groq's /openai, Azure's
// /openai/deployments/<d>.
const completionsSuffix = "/completions"

// dialectFor reports which dialect path speaks, from how it ends. One function for the
// request and every response-side call site, so a request and its response cannot be read
// as different dialects.
//
// THE END OF THE PATH, NOT THE WHOLE OF IT. Providers mount these APIs under prefixes of
// their own, and matching whole paths needed a code change for each one. What keeps an
// unrelated endpoint that happens to end the same way out of the inference record is the
// body check in parseOpenAIRequest and parseAnthropicRequest, not this function.
//
// path must already be query-free (endpointPath). A trailing slash is not trimmed: no
// client is known to send one, and TestInferenceParser_BobNonInferencePathsAreIgnored pins
// it as not inference.
func dialectFor(path string) dialect {
	switch {
	case strings.HasSuffix(path, anthropicMessagesPath):
		return dialectAnthropic
	case strings.HasSuffix(path, completionsSuffix):
		return dialectOpenAI
	}
	return dialectNone
}
