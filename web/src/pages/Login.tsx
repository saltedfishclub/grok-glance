import { useState } from "react";
import { Alert, Button, InputOTP } from "@heroui/react";
import { ApiError, api } from "../lib/api";
import { Centered } from "./Setup";

export function Login({ onDone }: { onDone: () => void }) {
  const [code, setCode] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (value: string) => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.login(value);
      onDone();
    } catch (cause) {
      setCode("");
      // 429 is worth naming separately: retrying immediately is exactly the
      // wrong response to it, and "wrong code" would invite precisely that.
      setError(
        cause instanceof ApiError && cause.status === 429
          ? "too many attempts — wait a moment before trying again"
          : cause instanceof ApiError && cause.status === 401
            ? "that code did not match"
            : cause instanceof Error
              ? cause.message
              : "sign-in failed",
      );
      setBusy(false);
    }
  };

  return (
    <Centered title="grok glance">
      {error ? (
        <Alert status="danger">
          <Alert.Content>
            <Alert.Description>{error}</Alert.Description>
          </Alert.Content>
        </Alert>
      ) : null}

      <p className="text-sm text-muted">Enter the current code from your authenticator.</p>

      <InputOTP
        maxLength={6}
        value={code}
        onChange={setCode}
        onComplete={submit}
        isDisabled={busy}
        isInvalid={error !== null}
        pattern="^\d*$"
        inputMode="numeric"
        aria-label="verification code"
        autoFocus
        className="self-center"
      >
        <InputOTP.Group>
          {[0, 1, 2, 3, 4, 5].map((index) => (
            <InputOTP.Slot key={index} index={index} />
          ))}
        </InputOTP.Group>
      </InputOTP>

      <Button fullWidth isDisabled={busy || code.length < 6} onPress={() => void submit(code)}>
        {busy ? "Checking…" : "Sign in"}
      </Button>
    </Centered>
  );
}
