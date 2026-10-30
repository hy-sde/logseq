import CoreGraphics
import Foundation

// Poll until a normal-layer, onscreen window owned by the given pid exists.
// Usage: winpoll <pid> <deadline_seconds>
// Prints elapsed milliseconds when found; exits 2 on deadline.

let args = CommandLine.arguments
guard args.count >= 2, let pid = Int32(args[1]) else { print("usage: winpoll pid [deadline]"); exit(3) }
let deadline = args.count > 2 ? (Double(args[2]) ?? 60.0) : 60.0
let t0 = Date()
while Date().timeIntervalSince(t0) < deadline {
    if let list = CGWindowListCopyWindowInfo([.optionOnScreenOnly], kCGNullWindowID) as? [[String: Any]] {
        for w in list {
            if let owner = w[kCGWindowOwnerPID as String] as? Int32, owner == pid,
               let layer = w[kCGWindowLayer as String] as? Int, layer == 0 {
                print(Int(Date().timeIntervalSince(t0) * 1000))
                exit(0)
            }
        }
    }
    usleep(50_000)
}
exit(2)
