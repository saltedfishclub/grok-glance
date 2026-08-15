import { useState } from "react";
import { Button, TextArea, TextField } from "@heroui/react";

/**
 * Prompt entry and the Stop button.
 *
 * Stop is deliberately always available while a turn runs, and is not merged
 * into the send control: interrupting is the one action a remote viewer most
 * urgently needs, and hunting for it inside a disabled composer is the wrong
 * experience at exactly the wrong moment.
 */
export function PromptBox({
  disabled,
  turnActive,
  onSend,
  onStop,
}: {
  disabled: boolean;
  turnActive: boolean;
  onSend: (text: string) => void;
  onStop: () => void;
}) {
  const [text, setText] = useState("");

  const send = () => {
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText("");
  };

  return (
    <div className="border-t border-border bg-background">
      <div className="mx-auto max-w-3xl px-4 py-3 flex items-end gap-2">
        <TextField
          className="grow"
          value={text}
          onChange={setText}
          isDisabled={disabled}
          aria-label="prompt"
        >
          <TextArea
            rows={1}
            placeholder={disabled ? "not connected" : "send a prompt…"}
            className="max-h-40 resize-none"
            onKeyDown={(event) => {
              // Enter sends, Shift+Enter breaks the line. `isComposing` guards
              // IME input: without it, committing a Chinese or Japanese
              // candidate with Enter would fire the prompt half-typed.
              if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
                event.preventDefault();
                send();
              }
            }}
          />
        </TextField>

        {turnActive ? (
          <Button variant="danger-soft" onPress={onStop} isDisabled={disabled}>
            Stop
          </Button>
        ) : null}

        <Button onPress={send} isDisabled={disabled || text.trim() === ""}>
          Send
        </Button>
      </div>
    </div>
  );
}
