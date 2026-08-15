import { useEffect, useState } from "react";
import QRCode from "qrcode";
import { Alert, Button, Card, InputOTP, Spinner } from "@heroui/react";
import { ApiError, api, type Enrollment } from "../lib/api";

/**
 * First-run TOTP enrolment.
 *
 * The bootstrap token in the query string is the whole access-control story
 * here: without it the server answers 404, which is what closes the window where
 * anyone who reaches the port first could enrol themselves as the owner. It is
 * spent the moment enrolment completes.
 */
export function Setup({ onDone }: { onDone: () => void }) {
  const token = new URLSearchParams(window.location.search).get("token") ?? "";

  const [enrollment, setEnrollment] = useState<Enrollment | null>(null);
  const [qr, setQr] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;

    api
      .setupBegin(token)
      .then(async (result) => {
        if (cancelled) return;
        setEnrollment(result);
        // A data: URI, not a remote image — the CSP allows `img-src 'self' data:`
        // precisely so this works without loosening anything for the rest of the
        // page. The secret never leaves the browser as a URL either way.
        const url = await QRCode.toDataURL(result.uri, { margin: 1, width: 220 });
        if (!cancelled) setQr(url);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setError(
          cause instanceof ApiError && cause.status === 404
            ? "that bootstrap token is not valid — it may already have been used"
            : cause instanceof Error
              ? cause.message
              : "enrolment could not be started",
        );
      });

    return () => {
      cancelled = true;
    };
  }, [token]);

  const submit = async (value: string) => {
    if (!enrollment || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.setupComplete(token, enrollment.secret, value);
      onDone();
    } catch (cause) {
      setCode("");
      setError(
        cause instanceof ApiError && cause.status === 401
          ? "that code did not match — check your device's clock and try the next one"
          : cause instanceof Error
            ? cause.message
            : "enrolment failed",
      );
      setBusy(false);
    }
  };

  if (!token) {
    return (
      <Centered title="Bootstrap token required">
        <p className="text-sm text-muted">
          glance printed a one-time setup link when it first started. Open that link, or read the
          token from <code className="glance-pre">~/.grok/glance/bootstrap.token</code> and visit{" "}
          <code className="glance-pre">/?token=…</code>.
        </p>
      </Centered>
    );
  }

  return (
    <Centered title="Set up two-factor sign-in">
      {error ? (
        <Alert status="danger">
          <Alert.Content>
            <Alert.Description>{error}</Alert.Description>
          </Alert.Content>
        </Alert>
      ) : null}

      {!enrollment ? (
        !error ? <Spinner color="accent" aria-label="preparing enrolment" /> : null
      ) : (
        <>
          <p className="text-sm text-muted">
            Scan this with your authenticator, then type the six digits it shows.
          </p>

          {qr ? (
            <img
              src={qr}
              alt="TOTP enrolment QR code"
              width={220}
              height={220}
              className="rounded-md bg-white p-2 self-center"
            />
          ) : null}

          <details className="text-xs text-muted">
            <summary className="cursor-pointer">Can't scan it?</summary>
            <p className="mt-2">Enter this secret by hand:</p>
            <code className="glance-pre block mt-1 break-all">{enrollment.secret}</code>
          </details>

          <InputOTP
            maxLength={6}
            value={code}
            onChange={setCode}
            onComplete={submit}
            isDisabled={busy}
            pattern="^\d*$"
            inputMode="numeric"
            aria-label="verification code"
            className="self-center"
          >
            <InputOTP.Group>
              {[0, 1, 2, 3, 4, 5].map((index) => (
                <InputOTP.Slot key={index} index={index} />
              ))}
            </InputOTP.Group>
          </InputOTP>

          <Button
            fullWidth
            isDisabled={busy || code.length < 6}
            onPress={() => void submit(code)}
          >
            {busy ? "Confirming…" : "Confirm"}
          </Button>
        </>
      )}
    </Centered>
  );
}

export function Centered({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="min-h-full flex items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <Card.Header>
          <Card.Title>{title}</Card.Title>
        </Card.Header>
        <Card.Content className="flex flex-col gap-4">{children}</Card.Content>
      </Card>
    </div>
  );
}
