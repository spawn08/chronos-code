#include <vector>
#include "repo.hpp"

namespace store {

/// Base repository.
class Repo : public Base, private Mixin {
public:
    Repo();
    virtual ~Repo();
    virtual int find(int id) const = 0;
    static Repo* create();

protected:
    int count_;

private:
    void reset();
};

struct Point {
    int x;
    int y;
    int norm() const { return x * x + y * y; }
};

template <typename T>
T max_of(T a, T b) {
    return a > b ? a : b;
}

Repo::Repo() : count_(0) {
    reset();
}

int Repo::find(int id) const {
    auto p = new Point();
    std::sort(items.begin(), items.end());
    return helper::lookup(id);
}

enum class Mode { Read, Write };

using IdList = std::vector<int>;

}  // namespace store

int main() {
    store::Repo r;
    return r.find(1);
}
