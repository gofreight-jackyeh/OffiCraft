// The login wall's second-factor mode + the credential-attempt brake.
//
// 🔴 WHAT THESE GUARD. The server answers ONE indistinguishable 401 for a wrong
// password and a wrong code — naming the failing half would confirm a correct
// password to an attacker who guessed only that half. The wall must therefore
// NOT claim to know which field is wrong. It is a silent regression to "fix"
// that copy into something more specific, so the wording is pinned here.
//
// The other pinned behaviour is 429: a rate-limited attempt is NOT a wrong
// credential, and telling the owner "wrong password" while they are merely
// throttled sends them hunting a typo that does not exist.

import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/react";
import { I18nProvider } from "../i18n";
import { zh } from "../i18n/locales/zh";
import { LoginPage } from "./LoginPage";
import { ApiError } from "../api/errors";
import { TOKEN_KEY } from "../api/auth";

const L = zh.login;

/** The composed 429 sentence, assembled exactly as i18n/compose.ts does. Built
 * from the same overridable leaves rather than hardcoded, so a wording change
 * moves the test with the UI instead of breaking it. */
function throttledText(secs: number): string {
  return `${L.throttledLead} ${secs} ${L.throttledTail}`;
}

function renderWall(mfaRequired: boolean, onSuccess = vi.fn()) {
  const utils = render(
    <I18nProvider>
      <LoginPage onSuccess={onSuccess} mfaRequired={mfaRequired} />
    </I18nProvider>,
  );
  return { ...utils, onSuccess };
}

/** Stub /api/login. `respond` returns [status, body, headers]. */
function stubLogin(
  respond: (body: unknown) => [number, unknown, Record<string, string>?],
) {
  const calls: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body ?? "{}"));
      calls.push(body);
      const [status, payload, headers = {}] = respond(body);
      return new Response(JSON.stringify(payload), {
        status,
        headers: { "Content-Type": "application/json", ...headers },
      });
    }),
  );
  return calls;
}

beforeEach(() => {
  localStorage.removeItem(TOKEN_KEY);
});

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.removeItem(TOKEN_KEY);
});

describe("LoginPage — no second factor (the control)", () => {
  it("renders ONLY a password field and sends no code", async () => {
    const calls = stubLogin(() => [200, { token: "tok" }]);
    const utils = renderWall(false);

    expect(utils.queryByLabelText(L.codePlaceholder)).toBeNull();

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    fireEvent.click(utils.getByText(L.submit));

    await waitFor(() => expect(utils.onSuccess).toHaveBeenCalled());
    // The field must be ABSENT from the body, not sent as "" or null: LoginDTO
    // is additionalProperties:false and this is the shape pre-MFA installs speak.
    expect(calls[0]).toEqual({ password: "pw" });
  });

  it("uses the password-only error copy on a 401", async () => {
    stubLogin(() => [401, { error: { code: "unauthorized", message: "x" } }]);
    const utils = renderWall(false);

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "wrong" },
    });
    fireEvent.click(utils.getByText(L.submit));

    await utils.findByText(L.error);
    expect(utils.queryByText(L.errorWithCode)).toBeNull();
    expect(utils.onSuccess).not.toHaveBeenCalled();
  });
});

describe("LoginPage — second factor armed", () => {
  it("renders a code field and submits both factors", async () => {
    const calls = stubLogin(() => [200, { token: "tok" }]);
    const utils = renderWall(true);

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    fireEvent.change(utils.getByLabelText(L.codePlaceholder), {
      target: { value: "123456" },
    });
    fireEvent.click(utils.getByText(L.submit));

    await waitFor(() => expect(utils.onSuccess).toHaveBeenCalled());
    expect(calls[0]).toEqual({ password: "pw", code: "123456" });
    expect(localStorage.getItem(TOKEN_KEY)).toBe("tok");
  });

  it("cannot submit until BOTH fields are filled", () => {
    stubLogin(() => [200, { token: "tok" }]);
    const utils = renderWall(true);
    const submit = utils.getByText(L.submit) as HTMLButtonElement;

    expect(submit.disabled).toBe(true);
    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    expect(submit.disabled).toBe(true); // password alone is not enough
    fireEvent.change(utils.getByLabelText(L.codePlaceholder), {
      target: { value: "123456" },
    });
    expect(submit.disabled).toBe(false);
  });

  // 🔴 The non-disclosure pin. The server cannot say which factor failed, so
  // neither can the wall — this copy names both on purpose.
  it("names BOTH factors in the error, never just one", async () => {
    stubLogin(() => [401, { error: { code: "unauthorized", message: "x" } }]);
    const utils = renderWall(true);

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    fireEvent.change(utils.getByLabelText(L.codePlaceholder), {
      target: { value: "000000" },
    });
    fireEvent.click(utils.getByText(L.submit));

    await utils.findByText(L.errorWithCode);
    expect(utils.queryByText(L.error)).toBeNull();
  });

  // The password is cleared (it may be the wrong one) but the code is NOT: it is
  // short-lived, so the next attempt needs a fresh one regardless, and wiping
  // the field just read off a phone is the more annoying half.
  it("clears the password but keeps the code after a failure", async () => {
    stubLogin(() => [401, { error: { code: "unauthorized", message: "x" } }]);
    const utils = renderWall(true);
    const pwd = utils.getByPlaceholderText(
      L.passwordPlaceholder,
    ) as HTMLInputElement;
    const code = utils.getByLabelText(L.codePlaceholder) as HTMLInputElement;

    fireEvent.change(pwd, { target: { value: "pw" } });
    fireEvent.change(code, { target: { value: "000000" } });
    fireEvent.click(utils.getByText(L.submit));

    await utils.findByText(L.errorWithCode);
    expect(pwd.value).toBe("");
    expect(code.value).toBe("000000");
  });
});

describe("LoginPage — the credential-attempt brake", () => {
  it("shows the throttle wait on a 429, NOT a wrong-credential error", async () => {
    stubLogin(() => [
      429,
      { error: { code: "client_error", message: "too many" } },
      { "Retry-After": "42" },
    ]);
    const utils = renderWall(false);

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    fireEvent.click(utils.getByText(L.submit));

    await utils.findByText(throttledText(42));
    // Crucially NOT the wrong-password copy — the password may have been right.
    expect(utils.queryByText(L.error)).toBeNull();
    expect(utils.queryByText(L.errorWithCode)).toBeNull();
  });

  it("falls back to 1s rather than 0s when Retry-After is missing", async () => {
    stubLogin(() => [429, { error: { code: "client_error", message: "x" } }]);
    const utils = renderWall(false);

    fireEvent.change(utils.getByPlaceholderText(L.passwordPlaceholder), {
      target: { value: "pw" },
    });
    fireEvent.click(utils.getByText(L.submit));

    // "retry in 0s" reads as "go ahead now", which a throttle must never say.
    await utils.findByText(throttledText(1));
  });
});

describe("login() error shape", () => {
  it("throws ApiError carrying the status and Retry-After", async () => {
    stubLogin(() => [
      429,
      { error: { code: "client_error", message: "too many" } },
      { "Retry-After": "7" },
    ]);
    const { login } = await import("../api/auth");

    await expect(login("pw")).rejects.toSatisfy((e: unknown) => {
      // The wall branches on these two, so a bare Error here would silently
      // collapse "throttled" into "wrong password".
      expect(e).toBeInstanceOf(ApiError);
      expect((e as ApiError).status).toBe(429);
      expect((e as ApiError).retryAfter).toBe(7);
      return true;
    });
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});
