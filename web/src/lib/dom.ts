//
// dom.ts
// Small hooks over the document that more than one component needs.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { RefObject } from "react";
import { useEffect, useRef } from "react";

/**
 * useDismiss closes a popover on an outside click or Escape. Both are needed: a
 * menu that only closes on Escape traps a mouse user, and one that only closes
 * on an outside click traps a keyboard user.
 *
 * Escape is taken on the capture phase and stopped there, because the page's
 * own Escape clears every filter, and closing a menu must not also throw away
 * the work underneath it.
 */
export function useDismiss(ref: RefObject<HTMLElement | null>, open: boolean, close: () => void): void {
	// The closer is read through a ref so an inline arrow from the caller does
	// not re-register the document listeners on every render.
	const closer = useRef(close);
	closer.current = close;

	useEffect(() => {
		if (!open) return;

		const onDown = (event: MouseEvent) => {
			if (ref.current && !ref.current.contains(event.target as Node)) closer.current();
		};

		const onKey = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;

			event.stopPropagation();
			closer.current();
		};

		document.addEventListener("mousedown", onDown);
		document.addEventListener("keydown", onKey, true);

		return () => {
			document.removeEventListener("mousedown", onDown);
			document.removeEventListener("keydown", onKey, true);
		};
	}, [open, ref]);
}
