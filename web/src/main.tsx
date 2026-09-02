//
// main.tsx
// The entry point: mount the dashboard.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./components/App";
import { ErrorBoundary } from "./components/ErrorBoundary";

/**
 * mount boots the SPA.
 *
 * A missing root element is a broken shell rather than a broken bundle, so it
 * fails with a sentence naming what is wrong instead of a null dereference that
 * sends whoever finds it into the minified bundle looking for a bug that is not
 * there. Everything below it sits inside one error boundary, because a render
 * error with nothing to catch it is a blank page with no message — the failure
 * mode this product promises never to have.
 */
function mount(): void {
	const root = document.getElementById("root");

	if (!root) {
		throw new Error("the dashboard shell is missing its #root element");
	}

	createRoot(root).render(
		<StrictMode>
			<ErrorBoundary>
				<App />
			</ErrorBoundary>
		</StrictMode>,
	);
}

mount();
