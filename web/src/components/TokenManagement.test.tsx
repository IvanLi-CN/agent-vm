import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import "../app/i18n";
import type { AgentToken, TokenPage } from "../app/types";

const listTokensMock = vi.hoisted(() => vi.fn());

vi.mock("../app/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../app/api")>();
  return { ...actual, listTokens: listTokensMock };
});

import { TokenManagement } from "./TokenManagement";

function token(tokenId: string): AgentToken {
  return { token_id: tokenId, resource_version: `token:${tokenId}:revision-1`, scopes: ["agent:read"], created_at: "2027-01-01T00:00:00.000Z", expires_at: "2027-01-15T00:00:00.000Z", revoked_at: null };
}

function renderTokenManagement() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><TokenManagement /></QueryClientProvider>);
}

describe("TokenManagement pagination", () => {
  it("moves through bounded token pages and returns to the first page", async () => {
    const user = userEvent.setup();
    const firstPage: TokenPage = { items: Array.from({ length: 25 }, (_, index) => token(`tok_page_01_${String(index).padStart(2, "0")}`)), page: { limit: 25, next_cursor: "mock-token:25" } };
    const secondPageTokenId = "tok_page_02_00";
    const secondPage: TokenPage = { items: [token(secondPageTokenId)], page: { limit: 25, next_cursor: null } };
    listTokensMock.mockReset();
    listTokensMock.mockResolvedValueOnce(firstPage).mockResolvedValueOnce(secondPage);

    renderTokenManagement();
    await waitFor(() => expect(screen.getByRole("button", { name: /下一页|next/i })).toBeEnabled());
    expect(screen.queryByText(secondPageTokenId)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /下一页|next/i }));
    expect(await screen.findByText(secondPageTokenId)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /上一页|previous/i })).toBeEnabled();

    await user.click(screen.getByRole("button", { name: /上一页|previous/i }));
    await waitFor(() => expect(screen.queryByText(secondPageTokenId)).not.toBeInTheDocument());
  });
});
