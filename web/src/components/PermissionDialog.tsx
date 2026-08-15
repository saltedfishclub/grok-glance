/**
 * The three interactions grok can block a turn on, rendered for the browser.
 *
 * These are *cards*, not modals. An interaction can be answered in the terminal
 * at any moment and will then vanish from here mid-read; a modal that steals
 * focus and then closes itself is far more jarring than a card that quietly
 * disappears from a tray. Cards also stack, which matters because more than one
 * can be open at once.
 *
 * Every response shape below is verbatim from grok's own wire types — the
 * agent deserializes into a typed struct and a near-miss is a hard error, so
 * these are not places to improvise:
 *
 *   session/request_permission -> acp::RequestPermissionResponse
 *   x.ai/ask_user_question     -> AskUserQuestionExtResponse  (tagged "outcome")
 *   x.ai/exit_plan_mode        -> ExitPlanModeExtResponse     ({outcome, feedback?})
 */

import { useState } from "react";
import {
  Button,
  Card,
  Checkbox,
  CheckboxGroup,
  Description,
  Label,
  Radio,
  RadioGroup,
  TextArea,
  TextField,
} from "@heroui/react";
import {
  METHOD_ASK_USER_QUESTION,
  METHOD_EXIT_PLAN_MODE,
  METHOD_REQUEST_PERMISSION,
  type AskUserQuestionParams,
  type ExitPlanModeParams,
  type Question,
  type RequestPermissionParams,
} from "../lib/acp";
import type { Interaction } from "../lib/ws";
import { ToolCall } from "./ToolCall";

const OPTION_VARIANT: Record<string, "primary" | "secondary" | "danger" | "danger-soft"> = {
  allow_once: "primary",
  allow_always: "secondary",
  reject_once: "danger-soft",
  reject_always: "danger",
};

function isMulti(question: Question): boolean {
  return question.multiSelect ?? question.multi_select ?? false;
}

function Shell({
  title,
  hint,
  children,
  footer,
}: {
  title: string;
  hint?: string;
  children?: React.ReactNode;
  footer: React.ReactNode;
}) {
  return (
    <Card className="border border-warning">
      <Card.Header>
        <Card.Title className="text-sm">{title}</Card.Title>
        {hint ? <Card.Description className="text-xs">{hint}</Card.Description> : null}
      </Card.Header>
      {children ? <Card.Content className="space-y-3">{children}</Card.Content> : null}
      <Card.Footer className="flex flex-wrap gap-2 justify-end">{footer}</Card.Footer>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// session/request_permission
// ---------------------------------------------------------------------------

function PermissionRequest({
  params,
  onAnswer,
  disabled,
}: {
  params: RequestPermissionParams;
  onAnswer: (result: unknown) => void;
  disabled: boolean;
}) {
  const options = params.options ?? [];
  return (
    <Shell
      title="Permission needed"
      hint="answering here also closes the prompt in the terminal"
      footer={
        <>
          <Button
            size="sm"
            variant="ghost"
            isDisabled={disabled}
            onPress={() => onAnswer({ outcome: { outcome: "cancelled" } })}
          >
            Dismiss
          </Button>
          {options.map((option) => (
            <Button
              key={option.optionId}
              size="sm"
              variant={OPTION_VARIANT[option.kind ?? ""] ?? "outline"}
              isDisabled={disabled}
              onPress={() =>
                onAnswer({ outcome: { outcome: "selected", optionId: option.optionId } })
              }
            >
              {option.name}
            </Button>
          ))}
        </>
      }
    >
      {params.toolCall ? <ToolCall call={params.toolCall} /> : null}
    </Shell>
  );
}

// ---------------------------------------------------------------------------
// x.ai/exit_plan_mode
// ---------------------------------------------------------------------------

function ExitPlanMode({
  params,
  onAnswer,
  disabled,
}: {
  params: ExitPlanModeParams;
  onAnswer: (result: unknown) => void;
  disabled: boolean;
}) {
  const [feedback, setFeedback] = useState("");
  const trimmed = feedback.trim();

  return (
    <Shell
      title="Plan ready for approval"
      hint="approve to let the agent start work, or send it back with notes"
      footer={
        <>
          <Button
            size="sm"
            variant="danger-soft"
            isDisabled={disabled}
            onPress={() => onAnswer({ outcome: "abandoned" })}
          >
            Abandon
          </Button>
          <Button
            size="sm"
            variant="ghost"
            isDisabled={disabled}
            onPress={() =>
              // `feedback` is only meaningful on "cancelled" — that is the one
              // path where the plan comes back for another round.
              onAnswer(trimmed ? { outcome: "cancelled", feedback: trimmed } : { outcome: "cancelled" })
            }
          >
            Keep planning
          </Button>
          <Button size="sm" isDisabled={disabled} onPress={() => onAnswer({ outcome: "approved" })}>
            Approve
          </Button>
        </>
      }
    >
      {params.planContent ? (
        // The plan is markdown. Rendering it properly would mean shipping a
        // markdown parser and sanitiser for text an agent wrote; showing the
        // source keeps it faithful and keeps the attack surface at zero.
        <div className="glance-prose text-sm max-h-96 overflow-auto rounded-md bg-surface-secondary p-3">
          {params.planContent}
        </div>
      ) : (
        <div className="text-sm text-muted">the agent sent no plan text</div>
      )}

      <TextField value={feedback} onChange={setFeedback} isDisabled={disabled} aria-label="feedback">
        <TextArea rows={2} placeholder="feedback (sent with “Keep planning”)" fullWidth />
      </TextField>
    </Shell>
  );
}

// ---------------------------------------------------------------------------
// x.ai/ask_user_question
// ---------------------------------------------------------------------------

interface QuestionState {
  labels: string[];
  other: boolean;
  notes: string;
}

const EMPTY_ANSWER: QuestionState = { labels: [], other: false, notes: "" };

/**
 * Build the `accepted` payload exactly as the TUI does
 * (xai-grok-pager/src/views/question_view.rs).
 *
 * The rules that are easy to get wrong and that grok's formatter depends on:
 * unanswered questions are *omitted* rather than sent empty; the map is keyed by
 * the question text, in the original order; a freeform-only answer is the literal
 * `["Other"]` with the typed text in `annotations[q].notes`; and `preview` is
 * carried only for single-select questions.
 */
function buildAccepted(questions: Question[], states: QuestionState[]) {
  const answers: Record<string, string[]> = {};
  const annotations: Record<string, { preview?: string; notes?: string }> = {};

  questions.forEach((question, index) => {
    const state = states[index] ?? EMPTY_ANSWER;
    const notes = state.other ? state.notes.trim() : "";
    const hasFreeform = state.other && notes !== "";
    if (state.labels.length === 0 && !hasFreeform) return;

    answers[question.question] = state.labels.length > 0 ? state.labels : ["Other"];

    const single = !isMulti(question);
    const selected = state.labels[0];
    const preview =
      single && state.labels.length === 1
        ? question.options.find((option) => option.label === selected)?.preview
        : undefined;

    if (preview || hasFreeform) {
      annotations[question.question] = {
        ...(preview ? { preview } : {}),
        ...(hasFreeform ? { notes } : {}),
      };
    }
  });

  return Object.keys(annotations).length > 0
    ? { outcome: "accepted", answers, annotations }
    : { outcome: "accepted", answers };
}

/** Plan-mode paths carry label-only partials; notes are dropped by design. */
function buildPartial(questions: Question[], states: QuestionState[]) {
  const partial: Record<string, string> = {};
  questions.forEach((question, index) => {
    const state = states[index] ?? EMPTY_ANSWER;
    const first = state.labels[0];
    if (first) partial[question.question] = first;
    else if (state.other && state.notes.trim() !== "") partial[question.question] = "Other";
  });
  return partial;
}

function QuestionCard({
  question,
  state,
  onChange,
  disabled,
}: {
  question: Question;
  state: QuestionState;
  onChange: (next: QuestionState) => void;
  disabled: boolean;
}) {
  const multi = isMulti(question);
  const preview =
    !multi && state.labels.length === 1
      ? question.options.find((option) => option.label === state.labels[0])?.preview
      : undefined;

  return (
    <div className="space-y-2">
      <div className="text-sm font-medium">{question.question}</div>

      {multi ? (
        <CheckboxGroup
          aria-label={question.question}
          value={state.labels}
          onChange={(labels) => onChange({ ...state, labels })}
          isDisabled={disabled}
          className="gap-1"
        >
          {question.options.map((option) => (
            <Checkbox key={option.label} value={option.label}>
              <Checkbox.Content>
                <Checkbox.Control>
                  <Checkbox.Indicator />
                </Checkbox.Control>
                <Label>{option.label}</Label>
              </Checkbox.Content>
              <Description>{option.description}</Description>
            </Checkbox>
          ))}
        </CheckboxGroup>
      ) : (
        <RadioGroup
          aria-label={question.question}
          value={state.labels[0] ?? ""}
          onChange={(label) => onChange({ ...state, labels: [label], other: false })}
          isDisabled={disabled}
          className="gap-1"
        >
          {question.options.map((option) => (
            <Radio key={option.label} value={option.label}>
              <Radio.Content>
                <Radio.Control>
                  <Radio.Indicator />
                </Radio.Control>
                <Label>{option.label}</Label>
              </Radio.Content>
              <Description>{option.description}</Description>
            </Radio>
          ))}
        </RadioGroup>
      )}

      {preview ? <pre className="glance-pre p-3 rounded-md bg-surface-secondary overflow-auto max-h-64">{preview}</pre> : null}

      {/* "Other" is not one of the model's options — it is the escape hatch the
          TUI also offers, and grok understands it by that exact spelling. */}
      <div className="space-y-1">
        <Checkbox
          isSelected={state.other}
          onChange={(other) =>
            onChange(multi ? { ...state, other } : { ...state, other, labels: other ? [] : state.labels })
          }
          isDisabled={disabled}
        >
          <Checkbox.Content>
            <Checkbox.Control>
              <Checkbox.Indicator />
            </Checkbox.Control>
            <Label>Other</Label>
          </Checkbox.Content>
        </Checkbox>

        {state.other ? (
          <TextField
            value={state.notes}
            onChange={(notes) => onChange({ ...state, notes })}
            isDisabled={disabled}
            aria-label="other"
            fullWidth
          >
            <TextArea rows={2} placeholder="your answer…" fullWidth />
          </TextField>
        ) : null}
      </div>
    </div>
  );
}

function AskUserQuestion({
  params,
  onAnswer,
  disabled,
}: {
  params: AskUserQuestionParams;
  onAnswer: (result: unknown) => void;
  disabled: boolean;
}) {
  const questions = params.questions ?? [];
  const [states, setStates] = useState<QuestionState[]>(() => questions.map(() => EMPTY_ANSWER));
  const planMode = params.mode === "plan";

  const answered = states.some(
    (state) => state.labels.length > 0 || (state.other && state.notes.trim() !== ""),
  );

  const update = (index: number, next: QuestionState) =>
    setStates((current) => current.map((state, i) => (i === index ? next : state)));

  return (
    <Shell
      title={questions.length > 1 ? `${questions.length} questions` : "A question for you"}
      hint="the terminal is showing this too — whoever answers first wins"
      footer={
        <>
          <Button
            size="sm"
            variant="ghost"
            isDisabled={disabled}
            onPress={() => onAnswer({ outcome: "cancelled" })}
          >
            Cancel
          </Button>
          {planMode ? (
            <>
              <Button
                size="sm"
                variant="outline"
                isDisabled={disabled}
                onPress={() =>
                  onAnswer({
                    outcome: "skip_interview",
                    partial_answers: buildPartial(questions, states),
                  })
                }
              >
                Skip &amp; plan
              </Button>
              <Button
                size="sm"
                variant="outline"
                isDisabled={disabled}
                onPress={() =>
                  onAnswer({
                    outcome: "chat_about_this",
                    partial_answers: buildPartial(questions, states),
                  })
                }
              >
                Chat about this
              </Button>
            </>
          ) : null}
          <Button
            size="sm"
            isDisabled={disabled || !answered}
            onPress={() => onAnswer(buildAccepted(questions, states))}
          >
            Submit
          </Button>
        </>
      }
    >
      {questions.map((question, index) => (
        <QuestionCard
          key={question.id ?? question.question}
          question={question}
          state={states[index] ?? EMPTY_ANSWER}
          onChange={(next) => update(index, next)}
          disabled={disabled}
        />
      ))}
    </Shell>
  );
}

// ---------------------------------------------------------------------------

export function PermissionDialog({
  interaction,
  onAnswer,
  onDecline,
}: {
  interaction: Interaction;
  onAnswer: (result: unknown) => void;
  /** For requests this build cannot render — replies with a JSON-RPC error. */
  onDecline: (reason: string) => void;
}) {
  // One answer per card. The card normally disappears when the server confirms,
  // but a lost race can take a moment, and a double-answer would be sent as a
  // second reply to a JSON-RPC id that is already resolved.
  const [sent, setSent] = useState(false);
  const answer = (result: unknown) => {
    if (sent) return;
    setSent(true);
    onAnswer(result);
  };

  switch (interaction.method) {
    case METHOD_REQUEST_PERMISSION:
      return (
        <PermissionRequest
          params={(interaction.params ?? {}) as RequestPermissionParams}
          onAnswer={answer}
          disabled={sent}
        />
      );

    case METHOD_EXIT_PLAN_MODE:
      return (
        <ExitPlanMode
          params={(interaction.params ?? {}) as ExitPlanModeParams}
          onAnswer={answer}
          disabled={sent}
        />
      );

    case METHOD_ASK_USER_QUESTION:
      return (
        <AskUserQuestion
          params={(interaction.params ?? {}) as AskUserQuestionParams}
          onAnswer={answer}
          disabled={sent}
        />
      );

    default:
      // grok's extension surface grows upstream. Guessing a response shape for
      // an unknown method would deserialize into garbage or hang the turn, so
      // this says so plainly and lets the terminal handle it.
      return (
        <Shell
          title={`Unsupported request: ${interaction.method}`}
          hint="answer this one in the terminal — this build does not know its response shape"
          footer={
            <Button
              size="sm"
              variant="ghost"
              isDisabled={sent}
              onPress={() => {
                if (sent) return;
                setSent(true);
                onDecline("no browser UI for " + interaction.method);
              }}
            >
              Decline here
            </Button>
          }
        >
          <pre className="glance-pre p-3 rounded-md bg-surface-secondary overflow-auto max-h-64">
            {JSON.stringify(interaction.params, null, 2)}
          </pre>
        </Shell>
      );
  }
}
