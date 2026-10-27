/** Minimal stroke icon set for navigation (16px, currentColor). */
const PATHS: Record<string, string> = {
  repos: "M3 3.5h4l1.5 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4.5a1 1 0 0 1 1-1Z",
  pipelines: "M4 3v10M4 8h5a3 3 0 0 0 3-3V3M12 13a1.5 1.5 0 1 0 0-.01M4 3a1.5 1.5 0 1 0 0 .01",
  changes: "M4 2.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM4 10.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM4 5.5v5M12 10.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM12 10.5V6a2 2 0 0 0-2-2H7.5M9 2.5 7.5 4 9 5.5",
  jobs: "M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01",
  environments: "M8 2 2 5l6 3 6-3-6-3ZM2 8l6 3 6-3M2 11l6 3 6-3",
  runners: "M2.5 3.5h11v4h-11zM2.5 9.5h11v4h-11zM5 5.5h.01M5 11.5h.01",
  recipes: "M3 2.5h7.5L13 5v8.5H3zM10.5 2.5V5H13M5.5 8h5M5.5 10.5h5",
  docs: "M8 4.5C6.5 3.3 4.5 3 2.5 3v9.5c2 0 4 .3 5.5 1.5 1.5-1.2 3.5-1.5 5.5-1.5V3c-2 0-4 .3-5.5 1.5Zm0 0V14",
  key: "M10 2.5a3.5 3.5 0 1 1-3.1 5.1L2.5 12v1.5H5V12h1.5v-1.5H8l.4-.4A3.5 3.5 0 0 1 10 2.5ZM11 5h.01",
  shield: "M8 1.8 2.8 3.8v4c0 3.1 2.2 5.5 5.2 6.4 3-.9 5.2-3.3 5.2-6.4v-4L8 1.8Z",
  audit: "M4 2.5h8v11H4zM6 5.5h4M6 8h4M6 10.5h2.5",
  lock: "M4 7.5h8v6H4zM5.5 7.5V5.5a2.5 2.5 0 0 1 5 0v2",
  sliders: "M3 4h6M12 4h1M3 8h1M7 8h6M3 12h7M13 12h0M10.5 2.5v3M5.5 6.5v3M11.5 10.5v3",
  search: "M7 12a5 5 0 1 0 0-10 5 5 0 0 0 0 10ZM14 14l-3.5-3.5",
  sun: "M8 11a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM8 1v1.5M8 13.5V15M1 8h1.5M13.5 8H15M3 3l1 1M12 12l1 1M3 13l1-1M12 4l1-1",
  moon: "M13.5 9.5A5.5 5.5 0 0 1 6.5 2.5a5.5 5.5 0 1 0 7 7Z",
  monitor: "M2 3h12v8H2zM6 14h4M8 11v3",
  collapse: "M10 4 6 8l4 4",
  expand: "M6 4l4 4-4 4",
  plus: "M8 3v10M3 8h10",
  play: "M5 3.5v9l7-4.5-7-4.5Z",
  bell: "M4 11V7a4 4 0 0 1 8 0v4l1.5 1.5h-11L4 11ZM6.5 13.5a1.5 1.5 0 0 0 3 0",
  more: "M8 3.5v.01M8 8v.01M8 12.5v.01",
  close: "M4 4l8 8M12 4l-8 8",
  menu: "M2.5 4.5h11M2.5 8h11M2.5 11.5h11",
  copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7",
  check: "M3.5 8.5 6.5 11.5 12.5 4.5",
  chevron: "M4 6l4 4 4-4",
  pause: "M6 4v8M10 4v8",
  drain: "M8 3v7M5 7.5 8 10.5l3-3M4 13h8",
  upgrade: "M8 13V6M5 8.5 8 5.5l3 3M4 3h8",
  trash: "M3 4.5h10M6.5 4.5V3h3v1.5M4.5 4.5l.5 9h6l.5-9",
  offline: "M8 8a2 2 0 1 0 0 .01M3 3l10 10",
  activity: "M2 8h2.5L6 4l4 8 1.5-4H14",
  dot: "M8 6a2 2 0 1 0 0 4 2 2 0 0 0 0-4Z",
  alert: "M8 2.5 14 13H2L8 2.5ZM8 6.5v3M8 11.2v.01",
  layers: "M8 2 2 5l6 3 6-3-6-3ZM2 8l6 3 6-3",
  merge: "M4 2.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM4 10.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM4 5.5v5M12 8.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM4 5.5c0 3 3 4.5 6.5 4.5",
  clock: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM8 4.5V8l2.5 1.5",
  blocked: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM3.8 3.8l8.4 8.4",
  closed: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM6 6l4 4M10 6l-4 4",
  ok: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM5.5 8.2l1.7 1.7 3.3-3.6",
  comment: "M2.5 3.5h11v7.5H8l-3 2.5V11H2.5z",
  file: "M4 2h5.5L12 4.5V14H4zM9.5 2v2.5H12",
  edit: "M10.5 2.5l3 3L6 13H3v-3z",
  external: "M9 2.5h4.5V7M13.5 2.5 7.5 8.5M11.5 9.5v4h-9v-9h4",
};

export type IconName = keyof typeof PATHS;

export default function Icon({ name, size = 16 }: { name: IconName; size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.4}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={PATHS[name]} />
    </svg>
  );
}
