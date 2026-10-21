// Package notify tells the human about loops (LLD §15): desktop
// notifications, a terminal sequence with the hook reply, ntfy (phone
// push), and Slack/Discord/generic webhooks. Sends are asynchronous, rate
// limited, and carry only what may leave the machine.
package notify
