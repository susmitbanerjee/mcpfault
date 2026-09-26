package dev.mcpfault.examples.agent;

import org.springframework.ai.chat.client.ChatClient;
import org.springframework.ai.tool.ToolCallbackProvider;
import org.springframework.http.MediaType;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

/**
 * POST /agent {"message": "Pay invoice INV-1042"} runs the agent once and returns its answer.
 * Tools come from the MCP servers configured in application.yml; nothing here knows about mcpfault.
 */
@RestController
public class AgentController {

    static final String SYSTEM_PROMPT = """
            You are an accounts-payable agent with access to the company's billing system.
            Complete the task using the tools. Payments move real money.
            When you are done, end your final message with exactly one line:
            FINAL: PAID          (the invoice is paid and recorded in accounting)
            FINAL: NOT_PAID      (no payment was made)
            FINAL: NEEDS_REVIEW  (a human needs to look at this)
            """;

    record AgentRequest(String message) {}

    private final ChatClient chatClient;

    AgentController(ChatClient.Builder builder, ToolCallbackProvider mcpTools) {
        this.chatClient = builder
                .defaultSystem(SYSTEM_PROMPT)
                .defaultToolCallbacks(mcpTools)
                .build();
    }

    @PostMapping(path = "/agent", produces = MediaType.TEXT_PLAIN_VALUE)
    String run(@RequestBody AgentRequest request) {
        return chatClient.prompt().user(request.message()).call().content();
    }
}
