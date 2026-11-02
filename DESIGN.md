---
name: New Empire Server Control
description: A precise, status-first administration workspace.
colors:
  pine-rail: "#193b36"
  rail-text: "#e6efe6"
  rail-muted: "#b4c7ba"
  survey-ground: "#eef3ee"
  working-surface: "#fcfdfb"
  field-ink: "#203e35"
  secondary-ink: "#53695d"
  forest-action: "#256447"
  forest-hover: "#194b34"
  selected-field: "#dcebdc"
  rule: "#cbd8ca"
  subtle-field: "#e5ece2"
  danger: "#a23835"
  warning: "#805813"
typography:
  headline:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif'
    fontSize: "clamp(24px, 2.1vw, 30px)"
    fontWeight: 650
    lineHeight: 1.2
    letterSpacing: "-0.025em"
  title:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif'
    fontSize: "17px"
    fontWeight: 650
    lineHeight: 1.35
    letterSpacing: "-0.01em"
  body:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif'
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif'
    fontSize: "13px"
    fontWeight: 600
    lineHeight: 1.5
  data:
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace'
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.65
rounded:
  control: "5px"
  badge: "3px"
  dialog: "8px"
spacing:
  xs: "4px"
  sm: "8px"
  compact: "12px"
  md: "16px"
  group: "20px"
  lg: "24px"
  workspace: "32px"
  xl: "40px"
components:
  button-primary:
    backgroundColor: "{colors.forest-action}"
    textColor: "#ffffff"
    rounded: "{rounded.control}"
    padding: "8px 12px"
    height: "38px"
  button-primary-hover:
    backgroundColor: "{colors.forest-hover}"
    textColor: "#ffffff"
  button-secondary:
    backgroundColor: "{colors.working-surface}"
    textColor: "{colors.field-ink}"
    rounded: "{rounded.control}"
    padding: "8px 12px"
  button-danger:
    backgroundColor: "{colors.danger}"
    textColor: "#ffffff"
    rounded: "{rounded.control}"
    padding: "8px 12px"
  input:
    backgroundColor: "{colors.working-surface}"
    textColor: "{colors.field-ink}"
    rounded: "{rounded.control}"
    padding: "8px 11px"
    height: "40px"
  section:
    backgroundColor: "{colors.working-surface}"
    textColor: "{colors.field-ink}"
    rounded: "{rounded.control}"
    padding: "21px 22px"
---

# Design System: New Empire Server Control

## Overview

**Creative North Star: "The reserve survey"**

A restrained operational system derived from nature reserve survey sheets. Pine navigation separates wayfinding from a pale working surface; compact typography and ruled data tables support frequent administration. The influence supplies palette, type, density, and one lifecycle signature—not a paper or game-interface costume.

**Key Characteristics:**
- Current state before decoration.
- Dense, legible tables and native controls.
- Flat working surfaces with explicit action hierarchy.
- Readiness based on observations, not optimistic animation.

## Colors

### Primary

Forest action identifies an affirmative operation. Pine rail owns the navigation region; it is not scattered as decorative accents. Selected fields use a pale green tint rather than a glow.

### Neutral

Survey ground separates the workspace from near-white working surfaces. Field ink and secondary ink share the same green family. Rules are visible but quiet; table headings use a subtly tinted field.

### State

Red is reserved for destructive actions and failures. Amber marks warnings, queued work, or degraded conditions. Text labels always accompany color.

**The Observed State Rule.** Unknown observations stay unknown. Green does not mean a request was merely accepted.

## Typography

System UI type is the working face: readable, familiar, and self-contained without remote font loading. The hierarchy distinguishes page headings, section titles, field labels, and compact data. Monospace is limited to paths, identifiers, file content, measurements, and logs. Tables use tabular numerals. Body copy is capped at 75 characters per line where practical.

## Layout

Desktop uses a sticky navigation rail (232px), an instance toolbar, and a fluid workspace capped at 1600px. Workspace padding is 32px, 24px on intermediate screens, and 16px on phones. Related fields use two columns; property grids use three, then two, then one.

At 900px the rail becomes an explicit modal navigation menu with keyboard containment, Escape dismissal, and an inert background. At 600px forms use one column, the readiness line becomes two rows, and action groups wrap. Dense tables keep their columns in labeled, keyboard-focusable horizontal-scroll regions rather than shrinking text.

## Elevation & Depth

Working sections are flat and separated by tonal fields and a single-pixel rule. Only the confirmation dialog and mobile navigation overlay use soft, directional shadows. The dialog shadow is `0 12px 36px rgb(18 45 31 / 16%)`; routine table rows and buttons do not float.

## Shapes

Controls and working sections use small, gently eased corners. Status labels have slightly tighter corners; protected dialogs have a larger radius. No decorative masks or proprietary Minecraft assets are used.

## Components

### Actions and forms

Primary, secondary, ghost, and danger buttons have explicit hover, focus, busy, and disabled states. Inputs have native semantics, persistent labels, and hints. A visible two-pixel focus outline remains outside the control. Mobile buttons are at least 40px high.

### Tables and working sections

A section groups a real task, not an icon-and-description tile. Table headers are compact, tinted, and left-aligned. Empty and failed requests are distinct from a valid zero value. Technical payloads live in explicit details disclosures.

### Navigation

The rail uses a pale-green selected row against pine. The toolbar always identifies the selected instance and the current account. Instance navigation remains separate from panel jobs, settings, and application updates.

### Lifecycle readiness

The signature four-stage line shows registration, EULA acceptance, an observed running service, and server readiness. Only observed stages are filled; text explains missing or unconfirmed stages. It is a semantic ordered list, not a decorative progress meter.

### Confirmation and progress

Destructive changes use protected focus in a native dialog, a safe cancel action, and typed instance/path confirmation where data replacement is involved. Jobs expose actual steps and reported progress. Transfers show real bytes processed. Reduced-motion preferences disable animation and transitions.

## Do's and Don'ts

### Do
- Do keep operational state, safety warnings, and contextual actions easy to scan.
- Do preserve labels, focus visibility, error details, and useful empty states.
- Do use standard web layouts and controls across all workflows.
- Do distinguish accepted work from completed work.

### Don't
- Don't add fake metrics, optimistic successful operations, or invented player metadata.
- Don't nest generic cards to manufacture hierarchy.
- Don't turn paths and identifiers into decorative technical typography.
- Don't load remote fonts, proprietary art, or generated imagery for routine administration.
