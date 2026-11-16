//
// Footer.tsx
// The line under every dashboard screen: who makes the product, and which build is running.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { ReactNode } from "react";

import type { Bootstrap } from "../api/types";
import { tx } from "../lib/i18n";

/** Where a release tag's notes and source are published. */
const RELEASES = "https://github.com/Feasible-Analytics/app.feasible.lol/releases/tag/";

/** A published release tag. A local `git describe` string carries a suffix
 *  after the tag, and an unstamped build says "dev"; neither has a page. */
const RELEASE_TAG = /^v\d+\.\d+\.\d+$/;

/** The company's legal name. It is not translated; the sentence around it is. */
const COMPANY = "Cloudmanic Labs, LLC";

/** The footer's links: the muted text of the line, underlined so they read as
 *  links without colour alone. */
const LINK =
	"underline decoration-line underline-offset-2 transition-colors duration-150 ease-[var(--ease-ui)] hover:text-body hover:decoration-current";

/** releaseURL is the GitHub release page for a version, or undefined when the
 *  version is not a published release tag. */
export function releaseURL(version: string): string | undefined {
	return RELEASE_TAG.test(version) ? RELEASES + version : undefined;
}

/**
 * WithFooter lays a dashboard screen out above the footer.
 *
 * The column is at least a screen tall so the footer sits at the bottom of a
 * short screen rather than under its last card. An embed gets the screen alone:
 * it is a component on somebody else's page and carries none of our chrome.
 */
export function WithFooter({ boot, children }: { boot: Bootstrap; children: ReactNode }) {
	if (boot.shared?.embed) return <>{children}</>;

	return (
		<div className="flex min-h-screen flex-col">
			{children}
			<Footer version={boot.version} />
		</div>
	);
}

/** Footer is the credit line and the running version. The version is printed
 *  exactly as it was stamped, and links to its release notes only when it is a
 *  published release. */
export function Footer({ version }: { version: string }) {
	const release = releaseURL(version);

	return (
		<footer className="mx-auto mt-auto w-full max-w-shell px-4 sm:px-5">
			<div className="flex flex-col gap-1 border-t-2 border-line py-4 text-xs text-muted sm:flex-row sm:items-center sm:justify-between sm:gap-4">
				<p>
					{tx("dashboard.footer.credit", {
						company: (
							<a href="https://cloudmanic.com" target="_blank" rel="noopener" className={LINK}>
								{COMPANY}
							</a>
						),
					})}
				</p>

				{version &&
					(release ? (
						<a href={release} target="_blank" rel="noopener" className={`tnum ${LINK}`}>
							{version}
						</a>
					) : (
						<span className="tnum">{version}</span>
					))}
			</div>
		</footer>
	);
}
