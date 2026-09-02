//
// ErrorBoundary.tsx
// The last thing between a render error and a blank page.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";

import { t } from "../lib/i18n";
import { Failure } from "./atoms";

interface Props {
	children: ReactNode;
}

interface State {
	message: string | null;
}

/**
 * ErrorBoundary catches a render error anywhere below it and shows the message
 * instead of the blank page React leaves behind when nothing catches one.
 *
 * It is a class because React only offers error boundaries to classes. Retry is
 * a full reload rather than a state reset: whatever threw was on the way to
 * painting, and re-rendering the same tree over the same URL would throw again.
 */
export class ErrorBoundary extends Component<Props, State> {
	state: State = { message: null };

	/** getDerivedStateFromError records the failure so the next render draws the
	 *  fallback. A thrown value with no message still gets a sentence rather
	 *  than an empty box. */
	static getDerivedStateFromError(error: unknown): State {
		return { message: error instanceof Error && error.message ? error.message : t("dashboard.error.query_failed") };
	}

	/** componentDidCatch keeps the stack in the console for whoever is
	 *  debugging; the screen shows only the sentence. */
	componentDidCatch(error: unknown, info: ErrorInfo): void {
		console.error(error, info.componentStack);
	}

	/** render draws the children until one of them throws. */
	render(): ReactNode {
		if (this.state.message === null) return this.props.children;

		return (
			<div className="h-screen bg-page">
				<Failure message={this.state.message} onRetry={() => location.reload()} />
			</div>
		);
	}
}
